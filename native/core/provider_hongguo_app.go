package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const hongguoAppBaseURL = "https://api5-normal-sinfonlineb.fqnovel.com"

const hongguoAppUserAgent = "com.phoenix.read/73532 (Linux; U; Android 16; zh_CN; 25053RT47C; Build/BP2A.250605.031.A3; Cronet/TTNetVersion:04657795 2026-01-23 QuicVersion:c67e9834 2025-09-08)"

type hongguoCatalogCursor struct {
	Offset        int       `json:"offset"`
	Pages         int       `json:"pages,omitempty"`
	StopReason    string    `json:"stopReason,omitempty"`
	SessionID     string    `json:"sessionId,omitempty"`
	LastID        string    `json:"lastId,omitempty"`
	PageSignature string    `json:"pageSignature,omitempty"`
	Initialized   bool      `json:"initialized"`
	Exhausted     bool      `json:"exhausted"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type hongguoCatalogState struct {
	Version       int                             `json:"version"`
	DeviceID      string                          `json:"deviceId"`
	InstallID     string                          `json:"installId"`
	Feeds         map[string]hongguoCatalogCursor `json:"feeds"`
	ScanStartedAt time.Time                       `json:"scanStartedAt,omitempty"`
}

type hongguoAppClient struct {
	mu                sync.Mutex
	catalogMu         sync.Mutex
	baseURL           string
	deviceRegisterURL string
	playbackDevice    hongguoPlaybackDevice
	devicePending     *hongguoDeviceCall
	webOnce           sync.Once
	webDownloader     *Downloader
	webPending        chan struct{}
	webRetryAt        time.Time
	state             hongguoCatalogState
	details           map[string]hongguoDetailEntry
	pending           map[string]*hongguoDetailCall
	searches          map[string]hongguoSearchEntry
	searchPending     map[string]*hongguoSearchCall
	searchSeasons     map[string]*hongguoSearchSeasonCursor
	suggestions       map[string]hongguoSuggestionEntry
	suggestPending    map[string]*hongguoSuggestionCall
}

func (downloader *Downloader) hongguoClient() *hongguoAppClient {
	downloader.hongguoOnce.Do(func() {
		downloader.hongguo = &hongguoAppClient{
			baseURL:           hongguoAppBaseURL,
			deviceRegisterURL: hongguoDeviceRegisterURL,
			state:             hongguoCatalogState{Version: 1, DeviceID: newHongguoDeviceID(), InstallID: newHongguoDeviceID(), Feeds: map[string]hongguoCatalogCursor{}},
			details:           map[string]hongguoDetailEntry{}, pending: map[string]*hongguoDetailCall{},
			searches: map[string]hongguoSearchEntry{}, searchPending: map[string]*hongguoSearchCall{},
			searchSeasons: map[string]*hongguoSearchSeasonCursor{},
			suggestions:   map[string]hongguoSuggestionEntry{}, suggestPending: map[string]*hongguoSuggestionCall{},
		}
	})
	return downloader.hongguo
}

func cloneHongguoCatalogState(state *hongguoCatalogState) *hongguoCatalogState {
	if state == nil {
		return nil
	}
	cloned := *state
	cloned.Feeds = make(map[string]hongguoCatalogCursor, len(state.Feeds))
	for name, cursor := range state.Feeds {
		cloned.Feeds[name] = cursor
	}
	return &cloned
}

func (downloader *Downloader) restoreHongguoCatalog(state *hongguoCatalogState) {
	if state == nil || state.Version != 1 || !hongguoNumericID.MatchString(state.DeviceID) || !hongguoNumericID.MatchString(state.InstallID) {
		return
	}
	cloned := cloneHongguoCatalogState(state)
	for name, cursor := range cloned.Feeds {
		known := false
		for _, genre := range hongguoAppGenres {
			known = known || name == genre.key || name == "category:"+genre.key
		}
		if !known || !cursor.Initialized && (cursor.Offset != 0 || cursor.Exhausted) || cursor.Offset < 0 || cursor.Offset > 1_000_000 || cursor.Pages < 0 || cursor.Pages > 1_000_000 || len(cursor.SessionID) > 4096 || len(cursor.LastID) > 256 || len(cursor.PageSignature) > 64 {
			delete(cloned.Feeds, name)
			continue
		}
		switch cursor.StopReason {
		case "", "end", "batch", "error", "canceled", "timeout":
		default:
			cursor.StopReason = ""
			cloned.Feeds[name] = cursor
		}
	}
	client := downloader.hongguoClient()
	client.catalogMu.Lock()
	defer client.catalogMu.Unlock()
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.state.Feeds) == 0 {
		client.state = *cloned
	}
}

func (downloader *Downloader) hongguoCatalogSnapshot() *hongguoCatalogState {
	client := downloader.hongguoClient()
	client.mu.Lock()
	defer client.mu.Unlock()
	return cloneHongguoCatalogState(&client.state)
}

func hongguoCatalogInitialized(state *hongguoCatalogState) bool {
	if state != nil && state.Version == 1 {
		for _, cursor := range state.Feeds {
			if cursor.Initialized {
				return true
			}
		}
	}
	return false
}

func hongguoCatalogHasMore(state *hongguoCatalogState) bool {
	for _, genre := range hongguoAppGenres {
		if state == nil || !state.Feeds[genre.key].Exhausted {
			return true
		}
	}
	return false
}

func (downloader *Downloader) hongguoAppRequest(ctx context.Context, method, path string, extra url.Values, payload any) (map[string]any, error) {
	client := downloader.hongguoClient()
	client.mu.Lock()
	base, deviceID, installID := client.baseURL, client.state.DeviceID, client.state.InstallID
	client.mu.Unlock()
	query := url.Values{
		"aid": {"8662"}, "app_name": {"novelread"}, "version_code": {"73532"}, "version_name": {"7.3.5.32"},
		"manifest_version_code": {"73532"}, "update_version_code": {"73532"}, "channel": {"update_64"},
		"device_platform": {"android"}, "os": {"android"}, "ssmix": {"a"}, "device_type": {"25053RT47C"},
		"device_brand": {"Redmi"}, "language": {"zh"}, "os_api": {"36"}, "os_version": {"16"},
		"resolution": {"1280*2772"}, "dpi": {"520"}, "ac": {"wifi"}, "device_id": {deviceID}, "iid": {installID},
	}
	for key, values := range extra {
		query[key] = append([]string(nil), values...)
	}
	catalog := path == hongguoAppCatalogPath
	userAgent := hongguoAppUserAgent
	if catalog {
		userAgent = hongguoPlaybackUserAgent
		for _, key := range []string{"version_code", "manifest_version_code", "update_version_code", "pv_player"} {
			query.Set(key, "71332")
		}
		query.Set("version_name", "7.1.3.32")
	}
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	attempts := downloader.cfg.Retries
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 3 {
		attempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", userAgent)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("X-XS-From-Web", "0")
		request.Header.Set("Sdk-Version", "2")
		if payload != nil {
			request.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
		var entropy hongguoPlaybackEntropy
		if catalog {
			entropy, err = newHongguoPlaybackEntropy()
			if err != nil {
				return nil, errors.New("红果 App 目录签名初始化失败")
			}
			request.Header.Del("X-XS-From-Web")
			request.Header.Set("X-SS-From-Web", "0")
			request.Header.Set("X-Tt-Request-Tag", "t=0;n=0")
			request.Header.Set("Passport-Sdk-Version", "50561")
			request.Header.Set("X-Vc-Bdturing-Sdk-Version", "3.7.2.cn")
		}
		response, err := downloader.doPreparedCatalogRequest(request, 20*time.Second, func(prepared *http.Request) {
			now := time.Now()
			query.Set("_rticket", strconv.FormatInt(now.UnixMilli(), 10))
			if catalog {
				query.Set("ts", strconv.FormatInt(now.Unix(), 10))
				prepared.URL.RawQuery = strings.ReplaceAll(strings.ReplaceAll(query.Encode(), "+", "%20"), "%2A", "*")
				signHongguoPlaybackRequest(prepared, body, deviceID, now, entropy)
				return
			}
			prepared.URL.RawQuery = query.Encode()
			signHongguoRequest(prepared, body, now)
		})
		if err != nil {
			lastErr = err
			var backoff *requestBackoff
			if errors.As(err, &backoff) || ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
		response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("红果 App 接口 HTTP %d", response.StatusCode)
			if response.StatusCode >= 400 && response.StatusCode < 500 {
				return nil, lastErr
			}
			continue
		}
		if len(content) > providerMaxBodyBytes {
			return nil, errors.New("红果 App 接口响应超过大小限制")
		}
		if len(bytes.TrimSpace(content)) == 0 {
			lastErr = errors.New("红果 App 接口 HTTP 200 返回空内容")
			continue
		}
		var result map[string]any
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		if err := decoder.Decode(&result); err != nil || result == nil {
			return nil, errors.New("红果 App 接口返回格式异常")
		}
		code := firstNonEmpty(mapString(result, "code", "Code", "status_code"), mapString(nestedMap(result, "BaseResp"), "StatusCode"))
		if code != "" && code != "0" {
			return nil, fmt.Errorf("红果 App 接口暂不可用（%s）", truncate(code, 20))
		}
		return result, nil
	}
	return nil, lastErr
}

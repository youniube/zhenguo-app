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
	"sort"
	"strconv"
	"strings"
	"time"
)

var errHongguoPlaybackSession = errors.New("红果高清播放会话暂不可用")

func (d *Downloader) hongguoPlaybackModel(ctx context.Context, videoID string) (map[string]any, error) {
	payload := map[string]any{
		"biz_param": map[string]any{
			"detail_page_version": 0, "device_level": 3, "disable_digg_stat": false,
			"need_all_video_definition": true, "need_mp4_align": false,
			"use_os_player": false, "use_server_dns": false, "video_platform": 1024,
		},
		"mixed_video_id_map": map[string]any{"1004": []string{videoID}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	client := d.hongguoClient()
	client.mu.Lock()
	base := client.baseURL
	client.mu.Unlock()
	attempts := max(1, min(d.cfg.Retries, 3))
	var lastErr error
	var rejectedID string
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 600 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		device, err := d.hongguoPlaybackSession(ctx, rejectedID)
		if err != nil {
			return nil, err
		}
		rejectedID = ""
		deviceID, installID := device.DeviceID, device.InstallID
		entropy, err := newHongguoPlaybackEntropy()
		if err != nil {
			return nil, errors.New("红果高清请求签名初始化失败")
		}
		query := url.Values{
			"iid": {installID}, "device_id": {deviceID}, "ac": {"wifi"}, "channel": {"update_64"},
			"aid": {"8662"}, "app_name": {"novelread"}, "version_code": {"71332"}, "version_name": {"7.1.3.32"},
			"device_platform": {"android"}, "os": {"android"}, "ssmix": {"a"}, "device_type": {"25053RT47C"},
			"device_brand": {"Redmi"}, "language": {"zh"}, "os_api": {"36"}, "os_version": {"16"},
			"manifest_version_code": {"71332"}, "resolution": {"1280*2772"}, "dpi": {"520"}, "update_version_code": {"71332"},
			"host_abi": {"arm64-v8a"}, "dragon_device_type": {"phone"}, "pv_player": {"71332"},
			"compliance_status": {"0"}, "need_personal_recommend": {"1"}, "player_so_load": {"1"}, "is_android_pad_screen": {"0"},
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/novel/player/multi_video_model/v1/", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", hongguoPlaybackUserAgent)
		request.Header.Set("Accept", "application/json; charset=utf-8,application/x-protobuf")
		request.Header.Set("Content-Type", "application/json; charset=UTF-8")
		request.Header.Set("X-XS-From-Web", "0")
		request.Header.Set("X-Tt-Request-Tag", "t=0;n=0")
		request.Header.Set("Sdk-Version", "2")
		request.Header.Set("Passport-Sdk-Version", "50561")
		request.Header.Set("X-Vc-Bdturing-Sdk-Version", "3.7.2.cn")
		request.Header.Set("X-Tt-Dt", "")
		response, err := d.doPreparedCatalogRequest(request, 30*time.Second, func(prepared *http.Request) {
			now := time.Now()
			query.Set("ts", strconv.FormatInt(now.Unix(), 10))
			query.Set("_rticket", strconv.FormatInt(now.UnixMilli(), 10))
			prepared.URL.RawQuery = strings.ReplaceAll(strings.ReplaceAll(query.Encode(), "+", "%20"), "%2A", "*")
			signHongguoPlaybackRequest(prepared, body, deviceID, now, entropy)
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
			lastErr = fmt.Errorf("红果高清接口 HTTP %d", response.StatusCode)
			if response.StatusCode >= 400 && response.StatusCode < 500 {
				return nil, lastErr
			}
			continue
		}
		model, err := parseHongguoPlaybackModel(content, videoID)
		if err == nil {
			return model, nil
		}
		lastErr = err
		if errors.Is(err, errHongguoPlaybackSession) {
			rejectedID = deviceID
			attempts = max(attempts, 2)
		}
	}
	return nil, lastErr
}

func parseHongguoPlaybackModel(content []byte, videoID string) (map[string]any, error) {
	if len(content) > providerMaxBodyBytes {
		return nil, errors.New("红果高清接口响应过大")
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, fmt.Errorf("%w：未返回有效数据", errHongguoPlaybackSession)
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || result == nil {
		return nil, fmt.Errorf("%w：响应格式异常", errHongguoPlaybackSession)
	}
	code := firstNonEmpty(mapString(result, "code", "Code", "status_code"), mapString(nestedMap(result, "BaseResp"), "StatusCode"))
	if code != "" && code != "0" {
		return nil, fmt.Errorf("红果高清接口暂不可用（%s）", truncate(code, 20))
	}
	entry := nestedMap(result, "data", videoID)
	var model map[string]any
	switch value := entry["video_model"].(type) {
	case map[string]any:
		model = value
	case string:
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&model); err != nil {
			return nil, errors.New("红果高清播放信息格式异常")
		}
	}
	if len(model) == 0 {
		return nil, fmt.Errorf("%w：未返回所请求分集的播放信息", errHongguoPlaybackSession)
	}
	if len(hongguoModelVariants(model)) == 0 && model["fallback_api"] == nil {
		return nil, fmt.Errorf("%w：播放模型没有清晰度", errHongguoPlaybackSession)
	}
	return model, nil
}

func (d *Downloader) resolveHongguoFallbackMedia(ctx context.Context, model map[string]any) (providerMedia, error) {
	address, _ := model["fallback_api"].(string)
	if value, ok := model["fallback_api"].(map[string]any); ok {
		address = mapString(value, "fallback_api")
	}
	address = strings.TrimSpace(address)
	if !isProviderHTTPMediaURL(address) {
		return providerMedia{}, errors.New("红果二次取流接口地址无效")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return providerMedia{}, err
	}
	request.Header.Set("User-Agent", hongguoPlaybackUserAgent)
	request.Header.Set("Accept", "application/json")
	response, err := d.doCatalogRequestWithTimeout(request, 20*time.Second)
	if err != nil {
		return providerMedia{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	if err != nil {
		return providerMedia{}, err
	}
	if response.StatusCode != http.StatusOK {
		return providerMedia{}, fmt.Errorf("红果二次取流接口 HTTP %d", response.StatusCode)
	}
	if len(body) == 0 || len(body) > providerMaxBodyBytes {
		return providerMedia{}, errors.New("红果二次取流接口未返回有效数据")
	}
	var result map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil {
		return providerMedia{}, errors.New("红果二次取流接口返回格式异常")
	}
	return hongguoFallbackMedia(model, nestedMap(result, "video_info", "data"))
}

func hongguoFallbackMedia(model, data map[string]any) (providerMedia, error) {
	seed, err := decodeHongguoBase64(mapString(data, "key_seed"))
	if err != nil || len(seed) == 0 || len(seed) > 4096 {
		return providerMedia{}, errors.New("红果播放地址的解密种子无效")
	}
	rows := nestedMap(data, "video_list")
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var variants []any
	for _, key := range keys {
		item, _ := rows[key].(map[string]any)
		var addresses []string
		var collect func(any)
		collect = func(raw any) {
			switch value := raw.(type) {
			case string:
				value = strings.TrimSpace(value)
				if !isProviderHTTPMediaURL(value) {
					value, _ = decryptHongguoSpadeURL(value, seed)
				}
				if isProviderHTTPMediaURL(value) {
					addresses = append(addresses, value)
				}
			case []any:
				for _, value := range value {
					collect(value)
				}
			}
		}
		for _, field := range []string{"main_url", "backup_url", "backup_url_1", "backup_url_2", "backup_urls", "url_list"} {
			collect(item[field])
		}
		if len(addresses) == 0 {
			continue
		}
		backups := make([]any, 0, len(addresses)-1)
		for _, address := range addresses[1:] {
			backups = append(backups, address)
		}
		meta := map[string]any{}
		for _, field := range []string{"codec_type", "vheight", "vwidth", "definition"} {
			meta[field] = firstNonEmpty(mapString(nestedMap(item, "video_meta"), field), mapString(item, field))
		}
		encryption := map[string]any{}
		for field, value := range nestedMap(item, "encrypt_info") {
			encryption[field] = value
		}
		for _, field := range []string{"spade_a", "encrypt", "encryption_method", "kid"} {
			if value, found := item[field]; found {
				encryption[field] = value
			}
		}
		variants = append(variants, map[string]any{
			"main_url": addresses[0], "backup_urls": backups, "video_meta": meta,
			"encrypt_info": encryption, "gear_des_key": item["gear_des_key"],
		})
	}
	media, err := selectHongguoAppMedia(map[string]any{"video_list": variants, "video_duration": firstNonEmpty(mapString(model, "video_duration", "duration"), mapString(data, "video_duration", "duration"))})
	if err != nil {
		return providerMedia{}, err
	}
	applyHeaders := func(media providerMedia) providerMedia {
		parsed, _ := url.Parse(media.URL)
		media.Referer = ""
		media.credentials = &providerMediaCredentials{origin: providerMediaOrigin(parsed), userAgent: hongguoPlaybackUserAgent, omitReferer: true}
		return media
	}
	media = applyHeaders(media)
	for index := range media.Variants {
		media.Variants[index] = applyHeaders(media.Variants[index])
	}
	return media, nil
}

package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

const hongguoWebUserAgent = "Mozilla/5.0 (Linux; Android 12) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

func (d *Downloader) hongguoWebSession(ctx context.Context) (*Downloader, error) {
	client := d.hongguoClient()
	client.webOnce.Do(func() {
		session := *d.client
		session.Jar, _ = cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		client.webDownloader = &Downloader{cfg: d.cfg, client: &session, limiter: d.limiter,
			proxyRouter: d.proxyRouter, diagnostics: d.diagnostics, providerHosts: map[string]string{}}
	})
	session := client.webDownloader
	address, err := url.Parse(d.providerBaseURL(sourceHongguo) + "/")
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, cookie := range session.client.Jar.Cookies(address) {
			if cookie.Name == "ttwid" && cookie.Value != "" {
				return session, nil
			}
		}
		client.mu.Lock()
		if time.Now().Before(client.webRetryAt) {
			client.mu.Unlock()
			return session, nil
		}
		if pending := client.webPending; pending != nil {
			client.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending:
				continue
			}
		}
		pending := make(chan struct{})
		client.webPending = pending
		client.mu.Unlock()

		bootstrapCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		bootstrapCtx = context.WithValue(bootstrapCtx, providerTextUserAgentKey{}, hongguoWebUserAgent)
		_, _ = session.fetchProviderText(bootstrapCtx, address.String(), address.String())
		cancel()
		client.mu.Lock()
		if ctx.Err() == nil {
			client.webRetryAt = time.Now().Add(30 * time.Second)
		}
		client.webPending = nil
		close(pending)
		client.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return session, nil
	}
}

func (d *Downloader) fetchHongguoSearchAPI(ctx context.Context, keyword string, page int) (hongguoSearchEntry, error) {
	session, err := d.hongguoWebSession(ctx)
	if err != nil {
		return hongguoSearchEntry{}, err
	}
	page = max(1, page)
	base := d.providerBaseURL(sourceHongguo)
	query := url.Values{"aid": {"8662"}, "query": {keyword}, "page": {strconv.Itoa(page)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/search/result?"+query.Encode(), nil)
	if err != nil {
		return hongguoSearchEntry{}, err
	}
	request.Header.Set("User-Agent", hongguoWebUserAgent)
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Referer", base+"/")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	response, err := session.doCatalogRequestWithTimeout(request, hongguoSearchPageTimeout)
	if err != nil {
		return hongguoSearchEntry{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes+1))
	if err != nil {
		return hongguoSearchEntry{}, err
	}
	if len(body) > providerMaxBodyBytes {
		return hongguoSearchEntry{}, errors.New("红果搜索响应过大")
	}
	if response.StatusCode != http.StatusOK || catalogResponseBlockReason(response, body) != "" {
		return hongguoSearchEntry{}, session.catalogResponseError(request, response, body)
	}
	var result map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || result == nil || decoder.Decode(new(any)) != io.EOF {
		return hongguoSearchEntry{}, errors.New("红果搜索接口未返回有效数据")
	}
	if query := mapString(result, "query"); query != "" && query != keyword {
		return hongguoSearchEntry{}, errors.New("红果搜索接口返回了其他搜索词的结果")
	}
	rows, valid := result["searchList"].([]any)
	if !valid {
		return hongguoSearchEntry{}, errors.New("红果搜索接口未返回有效结果")
	}
	entry := hongguoSearchEntry{Dramas: make([]Drama, 0, len(rows)), webPage: page}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		drama := hongguoDramaFromAny(row, "短剧")
		if drama.ID != "" && !seen[drama.ID] {
			seen[drama.ID] = true
			entry.Dramas = append(entry.Dramas, drama)
		}
	}
	if len(rows) > 0 && len(entry.Dramas) == 0 {
		return hongguoSearchEntry{}, errors.New("红果搜索结果中没有可识别的剧集")
	}
	entry.Total, _ = strconv.Atoi(mapString(result, "totalCount"))
	totalPages, _ := strconv.Atoi(mapString(result, "totalPages"))
	entry.webHasMore = result["hasMore"] == true || totalPages > page
	entry.Total = max(entry.Total, len(entry.Dramas))
	entry.Limited = entry.webHasMore
	return entry, nil
}

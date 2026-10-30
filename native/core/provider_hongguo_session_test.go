package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHongguoPlaybackRenewsEmptySessionAndReusesRegisteredDevice(t *testing.T) {
	var models, registrations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/register":
			registrations.Add(1)
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			header := nestedMap(body, "header")
			if request.Method != http.MethodPost || request.Header.Get("User-Agent") != hongguoAppUserAgent ||
				mapString(body, "magic_tag") != "ss_app_log" || mapString(header, "version_code") != "73532" ||
				len(mapString(header, "openudid")) != 16 || request.URL.Query().Get("req_id") == "" {
				t.Error("device registration does not match the supplied Python profile")
			}
			_, _ = io.WriteString(writer, `{"device_id_str":"8123456789012345678","install_id_str":"8234567890123456789"}`)
		case "/novel/player/multi_video_model/v1/":
			if models.Add(1) == 1 {
				_, _ = io.WriteString(writer, `{"data":{}}`)
				return
			}
			if request.URL.Query().Get("device_id") != "8123456789012345678" || request.URL.Query().Get("iid") != "8234567890123456789" {
				t.Error("playback did not reuse the newly registered device")
			}
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			ids, _ := nestedMap(body, "mixed_video_id_map")["1004"].([]any)
			if len(ids) != 1 {
				t.Error("playback request lost its episode ID")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{ids[0].(string): map[string]any{
				"video_model": map[string]any{"video_list": map[string]any{"720p": map[string]any{
					"main_url": "https://media.example.test/episode.mp4", "codec_type": "h264",
				}}},
			}}})
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := &Downloader{cfg: Config{Retries: 1}, client: server.Client(), limiter: newRequestLimiter(2, 0)}
	d.hongguoClient().baseURL = server.URL
	d.hongguoClient().deviceRegisterURL = server.URL + "/register"
	for _, videoID := range []string{"800001", "800002"} {
		media, err := d.resolveHongguoAppMedia(context.Background(), videoID)
		if err != nil || media.Quality != 720 {
			t.Fatalf("playback did not recover an empty model: %+v, %v", media, err)
		}
	}
	if models.Load() != 3 || registrations.Load() != 1 {
		t.Fatalf("unexpected model/registration counts: %d/%d", models.Load(), registrations.Load())
	}
}

func TestHongguoDeviceRefreshIsSharedAndWaitCanBeCanceled(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var registrations atomic.Int32
	d := sourceFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		if registrations.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return sourceFixtureResponse(request, http.StatusOK, `{"device_id_str":"8123456789012345678","install_id_str":"8234567890123456789"}`), nil
	})
	client := d.hongguoClient()
	client.playbackDevice = hongguoPlaybackDevice{DeviceID: "100000000000000001", InstallID: "100000000000000002"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := d.hongguoPlaybackSession(ctx, "100000000000000001")
		leader <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := d.hongguoPlaybackSession(canceled, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled waiter was not released: %v", err)
	}
	follower := make(chan hongguoPlaybackDevice, 1)
	go func() {
		device, _ := d.hongguoPlaybackSession(ctx, "100000000000000001")
		follower <- device
	}()
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	if device := <-follower; device.DeviceID != "8123456789012345678" || registrations.Load() != 1 {
		t.Fatalf("concurrent refresh did not reuse the registered device: %+v, %d", device, registrations.Load())
	}
}

func TestHongguoDeviceRegistrationFailureUsesFreshPair(t *testing.T) {
	d := sourceFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		return sourceFixtureResponse(request, http.StatusOK, `{"device_id_str":"0","install_id_str":"0"}`), nil
	})
	if _, err := d.registerHongguoPlaybackDevice(context.Background()); err == nil {
		t.Fatal("zero device IDs were accepted")
	}
	d.hongguoClient().playbackDevice = hongguoPlaybackDevice{DeviceID: "100000000000000001", InstallID: "100000000000000002"}
	device, err := d.hongguoPlaybackSession(context.Background(), "100000000000000001")
	if err != nil || len(device.DeviceID) != 18 || len(device.InstallID) != 18 || device.DeviceID == "100000000000000001" {
		t.Fatalf("registration failure did not use the script's fresh-device fallback: %+v, %v", device, err)
	}
}

func TestHongguoWebSearchBootstrapsCookieAndLoadsNextPage(t *testing.T) {
	var home, pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			home.Add(1)
			http.SetCookie(writer, &http.Cookie{Name: "ttwid", Value: "synthetic-hongguo-session", Path: "/"})
			_, _ = io.WriteString(writer, "<html></html>")
		case "/api/search/result":
			pages.Add(1)
			cookie, err := request.Cookie("ttwid")
			if err != nil || cookie.Value != "synthetic-hongguo-session" || request.URL.Query().Get("aid") != "8662" || request.URL.Query().Get("query") != "示例" {
				t.Error("search is missing its bootstrap cookie or query")
			}
			if request.URL.Query().Get("page") == "1" {
				_, _ = io.WriteString(writer, `{"searchList":[{"series_id":8123456789012345678,"series_name":"示例甲"}],"totalCount":2,"totalPages":2,"hasMore":true}`)
			} else {
				_, _ = io.WriteString(writer, `{"searchList":[{"series_id_str":"8234567890123456789","series_name":"示例乙"}],"totalCount":2,"totalPages":2,"hasMore":false}`)
			}
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := &Downloader{cfg: Config{Retries: 1, HongguoURL: server.URL}, client: server.Client(), limiter: newRequestLimiter(2, 0)}
	entry, err := d.fetchHongguoSearchPage(context.Background(), "示例")
	if err != nil || len(entry.Dramas) != 1 || entry.Dramas[0].SourceID != "8123456789012345678" || !entry.Limited {
		t.Fatalf("first search page was not parsed precisely: %+v, %v", entry, err)
	}
	d.rememberHongguoSearch("示例", entry)
	entry, more, err := d.searchHongguoDramasPaged(context.Background(), "示例", 2)
	if err != nil || len(entry.Dramas) != 2 || more || home.Load() != 1 || pages.Load() != 2 {
		t.Fatalf("search continuation did not reuse its cookie and append page 2: %+v, %v, %d/%d", entry, err, home.Load(), pages.Load())
	}
}

func TestHongguoWebSearchKeepsSSRWhenAPIReturnsNoRows(t *testing.T) {
	d := sourceFixtureDownloader(t, func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.Path {
		case "/api/search/result":
			body = `{"searchList":[],"totalCount":0,"totalPages":1,"hasMore":false}`
		case "/search/示例":
			body = `<script>window._ROUTER_DATA={"loaderData":{"search_(keyword)/page":{"isSuccess":true,"query":"示例","searchList":[{"video_data":{"series_id_str":"700001","series_name":"示例"}}],"totalCount":1}}}</script>`
		case "/":
			body = "<html></html>"
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
		}
		return sourceFixtureResponse(request, http.StatusOK, body), nil
	})
	entry, err := d.fetchHongguoSearchPage(context.Background(), "示例")
	if err != nil || len(entry.Dramas) != 1 || entry.Dramas[0].SourceID != "700001" {
		t.Fatalf("SSR search fallback was lost: %+v, %v", entry, err)
	}
}

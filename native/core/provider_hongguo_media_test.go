package core

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func hongguoWebMediaFixture(series, video string, limit int, addresses ...string) string {
	page := map[string]any{
		"vid": video, "series_id": series,
		"seriesDetail":   map[string]any{"accessible_episode_cnt": limit, "vid_list": []string{"800001", "800002"}},
		"publicVideoSeo": map[string]any{"available": true, "durationSeconds": 8, "width": 360, "height": 640},
	}
	router, _ := json.Marshal(map[string]any{"loaderData": map[string]any{
		"player_(series_id)/(vid)/layout": map[string]any{"vid": video, "series_id": series},
		"player_(series_id)/(vid)/page":   page,
	}})
	body := "<script>window._ROUTER_DATA=" + string(router) + ";</script>"
	for _, address := range addresses {
		node, _ := json.Marshal(map[string]any{"@type": "VideoObject", "contentUrl": address})
		body += `<script TYPE='application/ld+json'>` + string(node) + `</script>`
	}
	return body
}

func TestHongguoWebMediaUsesBoundPublicURLAndTrialGate(t *testing.T) {
	address := "https://media.example.test/video/public/800001/360p.mp4?token=a%2Fb&x=1"
	foreign := "https://media.example.test/video/public/800002/360p.mp4"
	body := hongguoWebMediaFixture("700001", "800001", 1, foreign, address, address)
	media, err := parseHongguoWebMedia(body, "700001", "800001", hongguoBaseURL+"/")
	if err != nil || media.URL != address || media.Quality != 360 || media.Duration != 8*time.Second || len(media.Variants) != 1 {
		t.Fatalf("current public playback fields not parsed: %+v, %v", media, err)
	}
	for _, sample := range []struct {
		name, body, series, video string
	}{
		{"wrong-series", body, "700002", "800001"},
		{"wrong-video", body, "700001", "800002"},
		{"foreign-jsonld", hongguoWebMediaFixture("700001", "800001", 1, foreign), "700001", "800001"},
		{"outside-trial", hongguoWebMediaFixture("700001", "800002", 1, foreign), "700001", "800002"},
		{"zero-trial", hongguoWebMediaFixture("700001", "800001", 0, address), "700001", "800001"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			if _, err := parseHongguoWebMedia(sample.body, sample.series, sample.video, hongguoBaseURL+"/"); err == nil {
				t.Fatal("wrong episode or inaccessible public media was accepted")
			}
		})
	}
	node, _ := json.Marshal(map[string]string{"@type": "VideoObject", "contentUrl": address})
	body = hongguoWebMediaFixture("700001", "800001", 1) + `<script type="application/ld+json">` + html.EscapeString(string(node)) + `</script>`
	if media, err := parseHongguoWebMedia(body, "700001", "800001", hongguoBaseURL+"/"); err != nil || media.URL != address {
		t.Fatalf("HTML-escaped JSON-LD failed: %+v, %v", media, err)
	}
}

func hongguoEncryptedURLFixture(address string, seed []byte) string {
	first := sha512.Sum512(seed)
	second := sha512.Sum512(append(first[:], hongguoSpadeConstants[:]...))
	block, _ := aes.NewCipher(second[:16])
	plain := []byte(address)
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(padding)}, padding)...)
	encrypted := make([]byte, 4+len(plain))
	copy(encrypted, []byte{0xa8, 0, 1, 0})
	cipher.NewCBCEncrypter(block, second[16:32]).CryptBlocks(encrypted[4:], plain)
	return base64.StdEncoding.EncodeToString(encrypted)
}

func TestHongguoSignedPlaybackHeadersQualitiesAndPlaylist(t *testing.T) {
	seed := []byte("synthetic-hongguo-address-seed")
	var base string
	modelRequests, fallbackRequests, mediaRequests, pageRequests := 0, 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/player/700001/800002":
			pageRequests++
			_, _ = io.WriteString(writer, hongguoWebMediaFixture("700001", "800002", 2, base+"/video/public/800002/360p.mp4"))
		case "/novel/player/multi_video_model/v1/":
			modelRequests++
			var payload map[string]any
			_ = json.NewDecoder(request.Body).Decode(&payload)
			ids, _ := nestedMap(payload, "mixed_video_id_map")["1004"].([]any)
			if request.Method != http.MethodPost || len(ids) != 1 || ids[0] != "800002" || payload["dr_scene"] != nil || mapString(nestedMap(payload, "biz_param"), "video_platform") != "1024" || len(nestedMap(payload, "biz_param")) != 8 {
				t.Errorf("wrong batch request: %s, %+v", request.Method, payload)
			}
			if request.Header.Get("X-Gorgon") == "" || request.Header.Get("X-SS-STUB") == "" || request.URL.Query().Get("host_abi") != "arm64-v8a" {
				t.Errorf("batch signature or query missing: %+v", request.Header)
			}
			for _, header := range []string{"X-Ladon", "X-Khronos", "X-Argus", "X-Gorgon", "X-Helios", "X-Medusa"} {
				if request.Header.Get(header) == "" {
					t.Errorf("signed playback header missing: %s", header)
				}
			}
			for _, key := range []string{"device_id", "iid"} {
				if value := request.URL.Query().Get(key); len(value) != 18 || !hongguoNumericID.MatchString(value) {
					t.Errorf("playback device ID must contain 18 digits: %s", key)
				}
			}
			if request.Header.Get("User-Agent") != hongguoPlaybackUserAgent || request.URL.Query().Get("version_code") != "71332" {
				t.Error("playback protocol does not match the supplied implementation")
			}
			model, _ := json.Marshal(map[string]any{"video_id": "800002", "video_duration": 8, "fallback_api": map[string]any{"fallback_api": base + "/fallback?token=a%2Fb"}})
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"800002": map[string]any{"video_model": string(model)}}})
		case "/fallback":
			fallbackRequests++
			if request.Header.Get("User-Agent") != hongguoPlaybackUserAgent || request.Header.Get("Referer") != "" || request.URL.RawQuery != "token=a%2Fb" {
				t.Errorf("fallback request changed its signed URL or headers: %+v, %s", request.Header, request.URL.RawQuery)
			}
			variant := func(path, codec string, height int) map[string]any {
				return map[string]any{"main_url": hongguoEncryptedURLFixture(base+path, seed), "codec_type": codec, "vheight": height}
			}
			best := variant("/best.m3u8", "h264", 1080)
			best["spade_a"] = "m78T+Vu+EvdZvhL1X7kR90G6DvVfpSbcXaUm3luiJdyloGNsfXFwd2A="
			best["encrypt"] = true
			best["backup_url"] = hongguoEncryptedURLFixture(base+"/backup.m3u8", seed)
			_ = json.NewEncoder(writer).Encode(map[string]any{"video_info": map[string]any{"data": map[string]any{
				"key_seed": base64.StdEncoding.EncodeToString(seed), "video_list": map[string]any{
					"main": best, "hevc": variant("/hevc.mp4", "hevc", 1080), "low": variant("/low.mp4", "h264", 720), "unsupported": variant("/bad.mp4", "bytevc2", 2160),
				},
			}}})
		case "/best.m3u8", "/part.ts":
			mediaRequests++
			_, hasReferer := request.Header["Referer"]
			if hasReferer || request.Header.Get("Origin") != "" || request.Header.Get("User-Agent") != hongguoPlaybackUserAgent {
				t.Errorf("media request lost its App header policy: %+v", request.Header)
			}
			if request.URL.Path == "/best.m3u8" {
				writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				_, _ = io.WriteString(writer, "#EXTM3U\n#EXT-X-TARGETDURATION:8\n#EXTINF:8,\npart.ts\n#EXT-X-ENDLIST\n")
			} else {
				writer.Header().Set("Content-Type", "video/mp2t")
				_, _ = writer.Write(bytes.Repeat([]byte{0x47}, 188))
			}
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	base = server.URL
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := engine.downloader
	d.client, d.limiter, d.cfg.Retries, d.cfg.HongguoURL = server.Client(), newRequestLimiter(3, 0), 1, base
	d.hongguoClient().baseURL = base
	media, err := d.resolveHongguoMedia(context.Background(), Task{DramaID: "hongguo:700001", Chapter: Chapter{Source: sourceHongguo, VideoURL: "hongguo-cenc://800002"}, Index: 2})
	if err != nil || media.URL != base+"/best.m3u8" || len(media.Variants) != 4 || media.Quality != 1080 || media.Duration != 8*time.Second || media.Playlist == "" || len(media.CENCKey) != 16 {
		t.Fatalf("batch playback did not preserve media and variants: %+v, %v", media, err)
	}
	if modelRequests != 1 || fallbackRequests != 1 || pageRequests != 0 {
		t.Fatalf("unexpected resolver attempts: batch=%d fallback=%d web=%d", modelRequests, fallbackRequests, pageRequests)
	}
	plan, err := engine.nativeOpenPlayback(context.Background(), nativePlaybackChoices(media, 1080))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Key != "00112233445566778899aabbccddeeff" {
		t.Fatal("CENC content key was lost between the provider and playback plan")
	}
	defer engine.stream.server.Close()
	defer engine.nativeReleasePlayback(plan.Session)
	response, err := http.Get(plan.URL)
	if err != nil {
		t.Fatal(err)
	}
	playlist, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, line := range strings.Split(string(playlist), "\n") {
		if strings.HasPrefix(line, "http://127.0.0.1:") {
			response, err := http.Get(line)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK || len(body) != 188 {
				t.Fatalf("local stream did not forward the selected media: %d, %d", response.StatusCode, len(body))
			}
		}
	}
	if mediaRequests != 2 {
		t.Fatalf("expected one playlist and one segment request, got %d", mediaRequests)
	}
}

func TestHongguoBatchResponseRejectsForeignEpisodeAndInvalidModels(t *testing.T) {
	for _, sample := range []struct{ name, raw string }{
		{"foreign-episode", `{"data":{"800002":{"video_model":{"video_id":"800002"}}}}`},
		{"invalid-model-json", `{"data":{"800001":{"video_model":"invalid"}}}`},
		{"empty-model", `{"data":{"800001":{"video_model":{}}}}`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { _, _ = io.WriteString(writer, sample.raw) }))
			defer server.Close()
			d := &Downloader{cfg: Config{Retries: 1}, client: server.Client(), limiter: newRequestLimiter(1, 0)}
			d.hongguoClient().baseURL = server.URL
			d.hongguoClient().deviceRegisterURL = server.URL + "/register"
			if _, err := d.resolveHongguoAppMedia(context.Background(), "800001"); err == nil {
				t.Fatal("invalid batch response was accepted")
			}
		})
	}
}

func TestHongguoBatchAcceptsMediaIDAndObjectModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"data":{"800001":{"video_model":{"video_id":"vod-file-abcdef","video_list":{"video_1":{"main_url":"https://media.example.test/episode.mp4","video_meta":{"codec_type":"h264","definition":"720p"}}}}}}}`)
	}))
	defer server.Close()
	d := &Downloader{cfg: Config{Retries: 1}, client: server.Client(), limiter: newRequestLimiter(1, 0)}
	d.hongguoClient().baseURL = server.URL
	media, err := d.resolveHongguoAppMedia(context.Background(), "800001")
	if err != nil || media.URL != "https://media.example.test/episode.mp4" || media.Quality != 720 {
		t.Fatalf("the underlying VOD file ID must not be treated as an episode ID: %+v, %v", media, err)
	}
}

func TestHongguoFallbackEncryptedMediaRequiresContentKey(t *testing.T) {
	for _, spade := range []string{"", "invalid-key"} {
		_, err := hongguoFallbackMedia(map[string]any{}, map[string]any{
			"key_seed": base64.StdEncoding.EncodeToString([]byte("fixture-seed")),
			"video_list": map[string]any{"video_1": map[string]any{
				"main_url": "https://media.example.test/encrypted.mp4", "codec_type": "h264", "vheight": 1080,
				"encrypt": true, "spade_a": spade,
			}},
		})
		if err == nil {
			t.Fatal("an encrypted video was accepted without a usable content key")
		}
	}
}

func TestHongguoSpadeRejectsMalformedOrWrongSeed(t *testing.T) {
	encoded := hongguoEncryptedURLFixture("https://media.example.test/real.mp4?token=a%2Fb", []byte("fixture-seed"))
	address, err := decryptHongguoSpadeURL(encoded, []byte("fixture-seed"))
	if err != nil || address != "https://media.example.test/real.mp4?token=a%2Fb" {
		t.Fatalf("signed address changed during decryption: %s, %v", address, err)
	}
	for _, value := range []string{"", "invalid", base64.StdEncoding.EncodeToString(make([]byte, 20)), encoded} {
		if _, err := decryptHongguoSpadeURL(value, []byte("wrong-seed")); err == nil {
			t.Fatal("invalid encrypted media address was accepted")
		}
	}
}

func TestHongguoSignedModelDynamicQualitiesAndEncryption(t *testing.T) {
	seed := []byte("synthetic-signed-model-seed")
	model := map[string]any{
		"video_duration": 8,
		"dynamic_video": map[string]any{
			"key_seed": base64.StdEncoding.EncodeToString(seed),
			"dynamic_video_list": map[string]any{
				"1080p": map[string]any{
					"main_url":   hongguoEncryptedURLFixture("https://media.example.test/high.mp4?token=a%2Fb", seed),
					"backup_url": hongguoEncryptedURLFixture("https://backup.example.test/high.mp4", seed),
					"codec_type": "h264", "encrypt": true,
					"spade_a": "m78T+Vu+EvdZvhL1X7kR90G6DvVfpSbcXaUm3luiJdyloGNsfXFwd2A=",
				},
				"720p":  map[string]any{"play_addr": map[string]any{"url_list": []any{"https://media.example.test/mid.mp4"}}},
				"360p":  map[string]any{"main_url": "https://media.example.test/low.mp4"},
				"2160p": map[string]any{"main_url": "https://media.example.test/unsupported.mp4", "codec_type": "bytevc2"},
			},
		},
	}
	media, err := selectHongguoAppMedia(model)
	if err != nil || media.URL != "https://media.example.test/high.mp4?token=a%2Fb" || media.Quality != 1080 || len(media.Variants) != 4 || len(media.CENCKey) != 16 {
		t.Fatalf("signed dynamic model lost quality, addresses or key: %+v, %v", media, err)
	}
	choice := nativePlaybackChoices(media, 720)
	if len(choice.media) != 1 || choice.media[0].Quality != 720 || choice.media[0].URL != "https://media.example.test/mid.mp4" {
		t.Fatalf("manual quality must select the actual returned variant: %+v", choice)
	}
	if media.credentials == nil || media.credentials.userAgent != "com.phoenix.read/71332" || media.Referer != "https://novel.snssdk.com/" {
		t.Fatal("signed media did not retain the supplied playback headers")
	}
}

func TestHongguoSignedPlaybackDoesNotFallBackToPublicVideo(t *testing.T) {
	requests, registrations := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/novel/player/multi_video_model/v1/":
			requests++
			_, _ = io.WriteString(writer, `{"data":{}}`)
		case "/register":
			registrations++
			_, _ = io.WriteString(writer, `{"device_id_str":"8123456789012345678","install_id_str":"8234567890123456789"}`)
		default:
			t.Errorf("failed signed playback must not silently select public low quality: %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := &Downloader{cfg: Config{Retries: 1, HongguoURL: server.URL}, client: server.Client(), limiter: newRequestLimiter(1, 0)}
	d.hongguoClient().baseURL = server.URL
	d.hongguoClient().deviceRegisterURL = server.URL + "/register"
	_, err := d.resolveHongguoMedia(context.Background(), Task{DramaID: "hongguo:700001", Chapter: Chapter{VideoURL: "hongguo-cenc://800001"}})
	if err == nil || requests != 2 || registrations != 1 || !strings.Contains(err.Error(), "高清接口") {
		t.Fatalf("failed signed playback must report its error after one session renewal: requests=%d, registrations=%d, error=%v", requests, registrations, err)
	}
}

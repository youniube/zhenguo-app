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
	"time"
)

const hongguoDeviceRegisterURL = "https://log.snssdk.com/service/2/device_register/"

type hongguoPlaybackDevice struct {
	DeviceID  string
	InstallID string
}

type hongguoDeviceCall struct {
	done   chan struct{}
	device hongguoPlaybackDevice
	err    error
}

func (d *Downloader) hongguoPlaybackSession(ctx context.Context, rejectedID string) (hongguoPlaybackDevice, error) {
	if err := ctx.Err(); err != nil {
		return hongguoPlaybackDevice{}, err
	}
	client := d.hongguoClient()
	client.mu.Lock()
	device := client.playbackDevice
	if pending := client.devicePending; pending != nil {
		client.mu.Unlock()
		select {
		case <-ctx.Done():
			return hongguoPlaybackDevice{}, ctx.Err()
		case <-pending.done:
			if (errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded)) && ctx.Err() == nil {
				return d.hongguoPlaybackSession(ctx, rejectedID)
			}
			return pending.device, pending.err
		}
	}
	if device.DeviceID != "" && (rejectedID == "" || device.DeviceID != rejectedID) {
		client.mu.Unlock()
		return device, nil
	}
	if rejectedID == "" {
		deviceID, installID, err := newHongguoPlaybackDeviceIDs()
		device = hongguoPlaybackDevice{DeviceID: deviceID, InstallID: installID}
		if err == nil {
			client.playbackDevice = device
		}
		client.mu.Unlock()
		return device, err
	}
	pending := &hongguoDeviceCall{done: make(chan struct{})}
	client.devicePending = pending
	client.mu.Unlock()

	device, err := d.registerHongguoPlaybackDevice(ctx)
	if err != nil && ctx.Err() == nil {
		deviceID, installID, randomErr := newHongguoPlaybackDeviceIDs()
		device, err = hongguoPlaybackDevice{DeviceID: deviceID, InstallID: installID}, randomErr
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	client.mu.Lock()
	if err == nil {
		client.playbackDevice = device
	}
	pending.device, pending.err = device, err
	client.devicePending = nil
	close(pending.done)
	client.mu.Unlock()
	return device, err
}

func (d *Downloader) registerHongguoPlaybackDevice(ctx context.Context) (hongguoPlaybackDevice, error) {
	openUDID, clientUDID, cdid, requestID := randomHex(8), uuidLike(), uuidLike(), uuidLike()
	if openUDID == "" || clientUDID == "" || cdid == "" || requestID == "" {
		return hongguoPlaybackDevice{}, errors.New("红果设备注册初始化失败")
	}
	now := time.Now().UnixMilli()
	body, err := json.Marshal(map[string]any{
		"magic_tag": "ss_app_log", "_gen_time": now,
		"header": map[string]any{
			"display_name": "红果免费短剧", "aid": 8662, "app_version": "7.3.5.32",
			"version_code": 73532, "manifest_version_code": 73532, "update_version_code": 73532,
			"channel": "update_64", "package": "com.phoenix.read", "app_language": "zh",
			"os": "Android", "os_version": "16", "os_api": 36, "device_model": "25053RT47C",
			"device_brand": "Redmi", "device_manufacturer": "Xiaomi", "cpu_abi": "arm64-v8a",
			"density_dpi": 520, "display_density": "xxhdpi", "resolution": "2772x1280",
			"language": "zh", "timezone": 8, "access": "wifi", "openudid": openUDID,
			"clientudid": clientUDID, "cdid": cdid, "region": "CN", "tz_name": "Asia/Shanghai",
			"tz_offset": 28800, "sim_region": "cn", "device_platform": "android",
			"not_request_sender": 0, "is_system_app": 0, "sdk_flavor": "china",
			"rom": "OS2.0", "rom_version": "BP2A.250605.031.A3",
		},
	})
	if err != nil {
		return hongguoPlaybackDevice{}, err
	}
	client := d.hongguoClient()
	client.mu.Lock()
	address := client.deviceRegisterURL
	client.mu.Unlock()
	query := url.Values{"req_id": {requestID}, "_rticket": {strconv.FormatInt(now, 10)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address+"?"+query.Encode(), bytes.NewReader(body))
	if err != nil {
		return hongguoPlaybackDevice{}, err
	}
	request.Header.Set("User-Agent", hongguoAppUserAgent)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := d.doCatalogRequestWithTimeout(request, 20*time.Second)
	if err != nil {
		return hongguoPlaybackDevice{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return hongguoPlaybackDevice{}, fmt.Errorf("红果设备注册 HTTP %d", response.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil {
		return hongguoPlaybackDevice{}, err
	}
	if len(content) > 64<<10 {
		return hongguoPlaybackDevice{}, errors.New("红果设备注册响应过大")
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || result == nil {
		return hongguoPlaybackDevice{}, errors.New("红果设备注册返回格式异常")
	}
	device := hongguoPlaybackDevice{
		DeviceID: mapString(result, "device_id_str", "device_id"), InstallID: mapString(result, "install_id_str", "install_id"),
	}
	for _, value := range []string{device.DeviceID, device.InstallID} {
		if !hongguoNumericID.MatchString(value) || strings.TrimLeft(value, "0") == "" {
			return hongguoPlaybackDevice{}, errors.New("红果设备注册未返回有效标识")
		}
	}
	return device, nil
}

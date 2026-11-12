// Command plugin is the Codex Gateway plugin for CLIProxyAPI, built with
// -buildmode=c-shared. This file is only the C ABI boundary: it copies bytes
// across, forwards host callbacks, and contains no policy. Everything that
// decides anything lives in internal/gatewayplugin and is tested there.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/wsw/codex-gateway/deploy/codex-compat/plugin/internal/gatewayplugin"
)

// maxMessageBytes bounds one message in either direction. Request bodies are
// never passed to this plugin, so real messages are a few kilobytes; the bound
// exists so a length can always be converted to a C int without overflow.
const maxMessageBytes = 64 << 20

var (
	instanceOnce sync.Once
	instance     *gatewayplugin.Plugin
)

func plugin() *gatewayplugin.Plugin {
	instanceOnce.Do(func() {
		gatewayplugin.SetLogSink(hostLog)
		instance = gatewayplugin.New(hostBridge{})
	})
	return instance
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if api == nil {
		return 1
	}
	C.store_host_api(host)
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	// Last line of defense: a panic that escaped here would end the CPA process.
	defer func() {
		if recover() != nil {
			code = 1
		}
	}()
	if method == nil {
		return 1
	}
	if uint64(requestLen) > maxMessageBytes {
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		// #nosec G103 -- copies the host's request buffer; its length was bounded above
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw := plugin().Handle(C.GoString(method), payload)
	if response == nil || len(raw) == 0 || len(raw) > maxMessageBytes {
		return 1
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return 1
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

type hostEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
}

var errHost = errors.New("host callback failed")

// callHost invokes one host callback. Errors carry no host text: callers log
// only the fact of failure.
func callHost(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > maxMessageBytes {
		return nil, errHost
	}
	cMethod := C.CString(method)
	// #nosec G103 -- frees the C string allocated on the line above
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(raw) > 0 {
		cPayload := C.CBytes(raw)
		if cPayload == nil {
			return nil, errHost
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	code := C.call_host_api(cMethod, requestPtr, C.size_t(len(raw)), &response)
	var out []byte
	oversized := uint64(response.len) > maxMessageBytes
	if response.ptr != nil && response.len > 0 && !oversized {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	var envelope hostEnvelope
	if code != 0 || oversized || json.Unmarshal(out, &envelope) != nil || !envelope.OK {
		return nil, errHost
	}
	return envelope.Result, nil
}

type hostBridge struct{}

func (hostBridge) AuthList() ([]gatewayplugin.AuthEntry, error) {
	result, err := callHost(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return nil, err
	}
	var listing struct {
		Files []gatewayplugin.AuthEntry `json:"files"`
	}
	if json.Unmarshal(result, &listing) != nil {
		return nil, errHost
	}
	return listing.Files, nil
}

func (hostBridge) AuthJSON(authIndex string) (json.RawMessage, error) {
	result, err := callHost(pluginabi.MethodHostAuthGet, map[string]string{"auth_index": authIndex})
	if err != nil {
		return nil, err
	}
	var document struct {
		JSON json.RawMessage `json:"json"`
	}
	if json.Unmarshal(result, &document) != nil || len(document.JSON) == 0 {
		return nil, errHost
	}
	return document.JSON, nil
}

func (hostBridge) ResetCooldown(authIndex string) error {
	_, err := callHost(pluginabi.MethodHostRoutingResetCooldown, map[string]string{"auth_index": authIndex})
	return err
}

func hostLog(message string) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{"level": "warn", "message": "codex-gateway plugin: " + message})
}

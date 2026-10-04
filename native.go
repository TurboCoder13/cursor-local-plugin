package main

/*
#include <stdint.h>
#include <stdlib.h>

// Public CLIProxyAPI ABI v1. No Go values cross this boundary.
typedef struct { void *ptr; size_t len; } cliproxy_buffer;
typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    int (*call)(void *, const char *, const uint8_t *, size_t, cliproxy_buffer *);
    void (*free_buffer)(void *, size_t);
} cliproxy_host_api;
typedef struct {
    uint32_t abi_version;
    int (*call)(char *, uint8_t *, size_t, cliproxy_buffer *);
    void (*free_buffer)(void *, size_t);
    void (*shutdown)(void);
} cliproxy_plugin_api;

extern int local_call(char *, uint8_t *, size_t, cliproxy_buffer *);
extern void local_free(void *, size_t);
extern void local_shutdown(void);
static cliproxy_host_api local_host;
static int setup(const cliproxy_host_api *h, cliproxy_plugin_api *p) {
    if (!h || !p || h->abi_version != 1 || !h->call || !h->free_buffer) return 1;
    local_host = *h;
    p->abi_version = 1;
    p->call = local_call;
    p->free_buffer = local_free;
    p->shutdown = local_shutdown;
    return 0;
}
static int host_rpc(const char *m, const uint8_t *b, size_t n, cliproxy_buffer *r) {
    return local_host.call(local_host.host_ctx, m, b, n, r);
}
static void host_release(cliproxy_buffer r) {
    if (r.ptr) local_host.free_buffer(r.ptr, r.len);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"sync"
	"unsafe"

	"cursor-local-plugin/internal/provider"
)

var nativeMu sync.RWMutex
var service = provider.NewService(hostCall)

func currentService() *provider.Service { nativeMu.RLock(); s := service; nativeMu.RUnlock(); return s }

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	if status := C.setup(host, api); status != 0 {
		return C.int(status)
	}
	service.Shutdown()
	service = provider.NewService(hostCall)
	return 0
}

//export local_call
func local_call(method *C.char, data *C.uint8_t, length C.size_t, output *C.cliproxy_buffer) C.int {
	if output == nil {
		return 1
	}
	output.ptr, output.len = nil, 0
	var response []byte
	ok := false
	if method == nil || length > provider.MaxEnvelopeBytes || (length > 0 && data == nil) {
		response = []byte(`{"ok":false,"error":{"code":"invalid_request","message":"invalid or oversized plugin envelope","http_status":400}}`)
	} else {
		request := C.GoBytes(unsafe.Pointer(data), C.int(length))
		response, ok = currentService().Call(C.GoString(method), request)
	}
	output.ptr = C.CBytes(response)
	output.len = C.size_t(len(response))
	if ok {
		return 0
	}
	return 1
}

//export local_free
func local_free(data unsafe.Pointer, _ C.size_t) {
	C.free(data)
}

//export local_shutdown
func local_shutdown() {
	currentService().Shutdown()
}

func hostCall(method string, value any) (json.RawMessage, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	name := C.CString(method)
	data := C.CBytes(body)
	defer C.free(unsafe.Pointer(name))
	defer C.free(data)
	var result C.cliproxy_buffer
	code := C.host_rpc(name, (*C.uint8_t)(data), C.size_t(len(body)), &result)
	defer C.host_release(result)
	if code != 0 || result.ptr == nil || result.len > provider.MaxEnvelopeBytes {
		return nil, errors.New("proxy host callback failed")
	}
	var reply struct {
		OK     bool
		Result json.RawMessage
	}
	if err := json.Unmarshal(C.GoBytes(result.ptr, C.int(result.len)), &reply); err != nil || !reply.OK {
		return nil, errors.New("proxy host rejected callback")
	}
	return reply.Result, nil
}

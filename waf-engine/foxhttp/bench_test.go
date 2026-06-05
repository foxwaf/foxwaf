package foxhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkHandleEcho(b *testing.B) {
	srv, err := New(Config{Inspector: NopInspector{}})
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.ProtoMajor = 1
	req.ProtoMinor = 1
	req.RemoteAddr = "127.0.0.1:12345"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		srv.handle(rec, req, ProtoHTTP11)
	}
}

func BenchmarkCtxPool(b *testing.B) {
	srv, err := New(Config{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := srv.ctxPool.Get().(*Ctx)
		c.reset()
		srv.ctxPool.Put(c)
	}
}

func BenchmarkClientIP(b *testing.B) {
	addrs := []string{"127.0.0.1:8080", "[::1]:443", "192.168.1.1:65535"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = clientIP(addrs[i%len(addrs)])
	}
}

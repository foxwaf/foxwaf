package foxhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkHandleNoInspector(b *testing.B) {
	srv, err := New(Config{Inspector: nil})
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

func BenchmarkHandleNopInspector(b *testing.B) {
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

func BenchmarkStdHTTPDirect(b *testing.B) {
	h := http.HandlerFunc(defaultEchoHandler)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.ProtoMajor = 1
	req.ProtoMinor = 1
	req.RemoteAddr = "127.0.0.1:12345"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

func BenchmarkDetectProto(b *testing.B) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.ProtoMajor = 2
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = detectProto(req)
	}
}

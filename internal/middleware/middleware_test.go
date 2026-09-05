package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSAllowsModelStatusResetAndExposesDiagnostics(t *testing.T) {
	h := CORS("http://127.0.0.1:3000")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/v1/models/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if methods := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(methods, "DELETE") {
		t.Fatalf("allow methods = %q", methods)
	}
	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	for _, header := range []string{"X-Trae-Function", "X-Trae-Model-Status", "X-Trae-Model-Filter"} {
		if !strings.Contains(exposed, header) {
			t.Fatalf("missing exposed header %s in %q", header, exposed)
		}
	}
}

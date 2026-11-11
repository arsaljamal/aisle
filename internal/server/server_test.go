package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arsaljamal/aisle/internal/catalog"
	"github.com/arsaljamal/aisle/internal/faults"
)

func TestKnownQueriesMatchLoosely(t *testing.T) {
	known := KnownQueries([]catalog.Query{{ID: 1, Text: "Salon  Chair"}}, catalog.Labels{1: {5: 2}})
	if _, ok := known[normalise(" salon chair ")]; !ok {
		t.Fatalf("got %v", known)
	}
}

func TestClampLimit(t *testing.T) {
	if clampLimit(0, 10) != 10 || clampLimit(500, 10) != 50 || clampLimit(3, 10) != 3 {
		t.Fatal("clamp")
	}
}

func TestBadDeploySwitchChangesVersion(t *testing.T) {
	srv, err := New(nil, nil, faults.New(), nil, "m")
	if err != nil {
		t.Fatal(err)
	}
	h := srv.HTTP()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/faults/bad-deploy", nil))
	if rec.Code != http.StatusOK || srv.Version() != BadVersion {
		t.Fatalf("code %d version %s", rec.Code, srv.Version())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/faults/bad-deploy?on=false", nil))
	if srv.Version() != GoodVersion {
		t.Fatal("version not restored")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/faults/nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown fault accepted: %d", rec.Code)
	}
}

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeSuggestion(t *testing.T) {
	if got := normalizeSuggestion("Hồ Tây – Đống Đa"); got != "ho tay dong đa" && got != "ho tay dong da" {
		t.Fatalf("unexpected Vietnamese normalization: %q", got)
	}
	aliases := suggestionAliases("Hà Nội")
	found := false
	for _, alias := range aliases {
		if alias == "hn" {
			found = true
		}
	}
	if !found {
		t.Fatal("Hà Nội should expose the HN alias")
	}
}

func TestArgon2idPasswordRoundTrip(t *testing.T) {
	service := NewPasswordService(Config{ArgonMemory: 8192, ArgonTime: 1, ArgonParallelism: 1, ArgonHashLength: 32})
	hash, err := service.Hash("Lapnv@2005")
	if err != nil {
		t.Fatal(err)
	}
	if !service.Verify(hash, "Lapnv@2005") {
		t.Fatal("valid password was rejected")
	}
	if service.Verify(hash, "wrong-password") {
		t.Fatal("invalid password was accepted")
	}
	if hash[:10] != "$argon2id$" {
		t.Fatalf("unexpected hash format: %s", hash)
	}
}

func TestNormalizeVietnameseSuggestions(t *testing.T) {
	if got := normalizeSuggestion("H\u1ed3 T\u00e2y \u2013 \u0110\u1ed1ng \u0110a"); got != "ho tay dong da" {
		t.Fatalf("unexpected Vietnamese normalization: %q", got)
	}
	aliases := suggestionAliases("H\u00e0 N\u1ed9i")
	found := false
	for _, alias := range aliases {
		if alias == "hn" {
			found = true
		}
	}
	if !found {
		t.Fatal("Ha Noi should expose the HN alias")
	}
}

func TestSuggestionRank(t *testing.T) {
	if suggestionRank("place:city", 0.2) <= suggestionRank("amenity:restaurant", 0.8) {
		t.Fatal("cities should rank ahead of similarly matched POIs")
	}
	if suggestionRank("water:lake", 0.1) <= suggestionRank("water:reservoir", 0.1) {
		t.Fatal("lakes should rank ahead of reservoirs")
	}
}

func TestSuggestionPlaceName(t *testing.T) {
	if got := suggestionPlaceName("H\u1ed3 T\u00e2y", "Th\u00e0nh ph\u1ed1 H\u00e0 N\u1ed9i"); got != "H\u1ed3 T\u00e2y, Th\u00e0nh ph\u1ed1 H\u00e0 N\u1ed9i" {
		t.Fatalf("unexpected place name: %q", got)
	}
	if got := suggestionPlaceName("H\u00e0 N\u1ed9i", ""); got != "H\u00e0 N\u1ed9i" {
		t.Fatalf("unexpected fallback place name: %q", got)
	}
}

func TestPasswordPolicy(t *testing.T) {
	if got := validatePassword("Lapnv@2005"); got != "" {
		t.Fatalf("expected valid password, got %q", got)
	}
	if got := validatePassword("short"); got == "" {
		t.Fatal("expected weak password rejection")
	}
}

func TestEdDSAAccessToken(t *testing.T) {
	config := Config{Issuer: "auth-service", Audience: "api-gateway", KeyID: "test-key", AccessTTL: 60, AllowEphemeral: true}
	service, err := NewTokenService(config)
	if err != nil {
		t.Fatal(err)
	}
	user := User{ID: "user-1", AuthVersion: 1, Scopes: []string{"map:read"}, Plan: "free", Role: "admin"}
	token, _, err := service.Issue(user, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := service.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != user.ID || claims.SessionID != "session-1" || claims.Role != "admin" || !hasScope(claims, "map:read") {
		t.Fatal("token claims did not round-trip")
	}
}

func TestTileProxyForwardsOnlySafeTilePaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/datas/asia_full/0/0/0.png" {
			t.Fatalf("unexpected worker path: %s", request.URL.Path)
		}
		if request.Header.Get("X-Request-Id") != "request-1" {
			t.Fatal("request id was not forwarded")
		}
		response.Header().Set("Cache-Control", "public, max-age=60")
		response.Header().Set("Content-Type", "image/png")
		_, _ = response.Write([]byte("tile"))
	}))
	defer upstream.Close()

	server := &Server{config: Config{TileServerURL: upstream.URL}, workerClient: upstream.Client()}
	request := httptest.NewRequest(http.MethodGet, "/api/tiles/datas/asia_full/0/0/0.png", nil)
	request.Header.Set("X-Request-Id", "request-1")
	response := httptest.NewRecorder()
	server.tileProxy(response, request, "/datas/asia_full/0/0/0.png")
	if response.Code != http.StatusOK || response.Body.String() != "tile" {
		t.Fatalf("unexpected tile response: status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatal("cache header was not forwarded")
	}

	blocked := httptest.NewRecorder()
	server.tileProxy(blocked, httptest.NewRequest(http.MethodGet, "/api/tiles/../secret", nil), "/../secret")
	if blocked.Code != http.StatusNotFound {
		t.Fatalf("unsafe path returned %d, want 404", blocked.Code)
	}
}

func TestGeoDataProxyAllowsAnonymousAccessToPublicReleases(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/catalog" {
			t.Fatalf("unexpected GeoData path: %s", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("anonymous request unexpectedly included an authorization header")
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"items":[{"id":"public-map"}]}`))
	}))
	defer upstream.Close()

	server := &Server{config: Config{GeoDataURL: upstream.URL}, workerClient: upstream.Client()}
	request := httptest.NewRequest(http.MethodGet, "/api/geodata/catalog", nil)
	response := httptest.NewRecorder()

	server.geoDataProxy(response, request, "/v1/catalog")

	if response.Code != http.StatusOK || response.Body.String() != `{"items":[{"id":"public-map"}]}` {
		t.Fatalf("unexpected GeoData response: status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestValidSharedMapState(t *testing.T) {
	valid := sharedMapState{
		BaseMapID:  "asia_full",
		OverlayIDs: []string{"vietnam-osm", "administrative.boundaries"},
	}
	if !validSharedMapState(valid) {
		t.Fatal("valid shared map state was rejected")
	}
	if validSharedMapState(sharedMapState{OverlayIDs: []string{"vietnam-osm", "vietnam-osm"}}) {
		t.Fatal("duplicate overlay ids were accepted")
	}
	if validSharedMapState(sharedMapState{BaseMapID: "https://example.com/tiles"}) {
		t.Fatal("an arbitrary tile URL was accepted as a dataset id")
	}
}

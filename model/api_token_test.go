package model

import (
	"strings"
	"testing"
	"time"
)

func TestHashAPIToken_DeterministicAndAvalanche(t *testing.T) {
	a := HashAPIToken("nzp_alpha")
	b := HashAPIToken("nzp_alpha")
	if a != b {
		t.Fatalf("hash must be deterministic for identical inputs")
	}
	c := HashAPIToken("nzp_alphb")
	if a == c {
		t.Fatalf("single-byte change in input must change hash")
	}
	if len(a) != 64 {
		t.Fatalf("hash must be 64 hex chars (sha256), got %d", len(a))
	}
}

func TestAPIToken_HasScope_AllAndExact(t *testing.T) {
	tok := &APIToken{}
	tok.SetScopes([]string{ScopeServerRead, ScopeServerExec})
	if !tok.HasScope(ScopeServerRead) {
		t.Fatalf("explicit scope must pass")
	}
	if !tok.HasScope(ScopeServerExec) {
		t.Fatalf("explicit scope must pass")
	}
	if tok.HasScope(ScopeServerWrite) {
		t.Fatalf("missing scope must fail")
	}

	tok.SetScopes([]string{ScopeNezhaAll})
	for _, s := range []string{ScopeServerRead, ScopeServerWrite, ScopeServerDelete, ScopeServerExec} {
		if !tok.HasScope(s) {
			t.Fatalf("nezha:* must cover %s", s)
		}
	}
}

func TestAPIToken_HasScope_TrimsWhitespace(t *testing.T) {
	tok := &APIToken{ScopesCSV: " nezha:server:read , nezha:server:exec "}
	if !tok.HasScope(ScopeServerRead) {
		t.Fatalf("scope with surrounding whitespace must be normalized")
	}
}

func TestAPIToken_CanAccessServer_EmptyMeansAll(t *testing.T) {
	tok := &APIToken{}
	if !tok.CanAccessServer(1) {
		t.Fatalf("empty server list must allow any server")
	}
	if !tok.CanAccessServer(99999) {
		t.Fatalf("empty server list must allow any server")
	}
}

func TestAPIToken_CanAccessServer_Whitelist(t *testing.T) {
	tok := &APIToken{}
	tok.SetServerIDs([]uint64{2, 5, 7})
	if !tok.CanAccessServer(5) {
		t.Fatalf("listed server must be allowed")
	}
	if tok.CanAccessServer(6) {
		t.Fatalf("unlisted server must be denied")
	}
}

// List and stream projections ask once per server, so a lookup must not decode
// ServersCSV again: 1000 ids x 10k servers per /ws/server frame otherwise.
func TestAPIToken_CanAccessServer_DecodesWhitelistOnce(t *testing.T) {
	ids := make([]uint64, 1000)
	for i := range ids {
		ids[i] = uint64(len(ids) - i) // stored unsorted
	}
	created := &APIToken{TokenHash: HashAPIToken("nzp_whitelist")}
	created.SetServerIDs(ids)
	db := newMigrationTestDB(t)
	if err := db.Create(created).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	// The auth middleware hands out the row as loaded from the database.
	loaded := &APIToken{}
	if err := db.First(loaded, created.ID).Error; err != nil {
		t.Fatalf("load: %v", err)
	}

	for name, tok := range map[string]*APIToken{"created": created, "loaded": loaded} {
		if !tok.CanAccessServer(1) || !tok.CanAccessServer(1000) || tok.CanAccessServer(1001) {
			t.Fatalf("%s: whitelist 1..1000 must allow 1 and 1000 and reject 1001", name)
		}
		if allocs := testing.AllocsPerRun(100, func() { tok.CanAccessServer(1001) }); allocs != 0 {
			t.Fatalf("%s: CanAccessServer must not re-decode server_ids per call, got %v allocs", name, allocs)
		}
	}
}

func TestAPIToken_CanAccessServer_FollowsServersCSVChanges(t *testing.T) {
	tok := &APIToken{}
	tok.SetServerIDs([]uint64{1})
	if !tok.CanAccessServer(1) || tok.CanAccessServer(2) {
		t.Fatalf("whitelist {1} must allow only 1")
	}
	tok.SetServerIDs([]uint64{2})
	if tok.CanAccessServer(1) || !tok.CanAccessServer(2) {
		t.Fatalf("a changed whitelist must not be answered from the old one")
	}
	tok.ServersCSV = "3"
	if tok.CanAccessServer(2) || !tok.CanAccessServer(3) {
		t.Fatalf("a directly assigned ServersCSV must not be answered from the old whitelist")
	}
	tok.ServersCSV = ""
	if !tok.CanAccessServer(1) {
		t.Fatalf("a cleared whitelist must be unrestricted again")
	}
}

func TestAPIToken_SetServerIDs_RoundTrip(t *testing.T) {
	tok := &APIToken{}
	tok.SetServerIDs([]uint64{10, 11, 12})
	got := tok.ServerIDs()
	if len(got) != 3 || got[0] != 10 || got[2] != 12 {
		t.Fatalf("round-trip failed: %v", got)
	}
}

func TestAPIToken_ServerIDs_SkipsGarbage(t *testing.T) {
	tok := &APIToken{ServersCSV: "1,2,abc,3"}
	got := tok.ServerIDs()
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("garbage entries must be skipped; got %v", got)
	}
}

func TestAPIToken_IsExpired(t *testing.T) {
	tok := &APIToken{}
	if tok.IsExpired(time.Now()) {
		t.Fatalf("nil expiry must mean never expired")
	}
	past := time.Now().Add(-time.Hour)
	tok.ExpiresAt = &past
	if !tok.IsExpired(time.Now()) {
		t.Fatalf("past expiry must mark expired")
	}
	future := time.Now().Add(time.Hour)
	tok.ExpiresAt = &future
	if tok.IsExpired(time.Now()) {
		t.Fatalf("future expiry must not mark expired")
	}
}

func TestAPIToken_HashAPIToken_NoSecretLeak(t *testing.T) {
	plaintext := "nzp_supersecret"
	hash := HashAPIToken(plaintext)
	if strings.Contains(hash, plaintext) {
		t.Fatalf("hash must not contain plaintext")
	}
	if strings.Contains(hash, "super") {
		t.Fatalf("hash must not contain secret substring")
	}
}

func TestAPIToken_BeforeCreate_RejectsEmptyHash(t *testing.T) {
	tok := &APIToken{Name: "x"}
	err := tok.BeforeCreate(nil)
	if err == nil {
		t.Fatalf("BeforeCreate must reject empty TokenHash")
	}
}

func TestAPIToken_BeforeCreate_AcceptsNonEmptyHash(t *testing.T) {
	tok := &APIToken{Name: "x", TokenHash: HashAPIToken("nzp_xyz")}
	if err := tok.BeforeCreate(nil); err != nil {
		t.Fatalf("BeforeCreate must accept non-empty hash, got %v", err)
	}
}

func TestAPIToken_ToView_OmitsTokenHash(t *testing.T) {
	tok := &APIToken{
		ID:        1,
		UserID:    2,
		Name:      "x",
		TokenHash: "DEADBEEF",
	}
	tok.SetScopes([]string{ScopeServerRead})
	v := tok.ToView()
	if v.ID != 1 || v.Name != "x" {
		t.Fatalf("view missing core fields")
	}
	if len(v.Scopes) != 1 || v.Scopes[0] != ScopeServerRead {
		t.Fatalf("view missing scopes")
	}
}

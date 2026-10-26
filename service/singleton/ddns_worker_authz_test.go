package singleton

import (
	"testing"

	"github.com/nezhahq/nezha/model"
)

// newDDNSClassForTest builds a DDNSClass backed by an in-memory profile map,
// mirroring the production cache layout without touching the database.
func newDDNSClassForTest(profiles ...*model.DDNSProfile) *DDNSClass {
	list := make(map[uint64]*model.DDNSProfile, len(profiles))
	for _, p := range profiles {
		list[p.ID] = p
	}
	return &DDNSClass{
		class: class[uint64, *model.DDNSProfile]{
			list:       list,
			sortedList: profiles,
		},
	}
}

// GHSA-39g2-8x68-pmx8: a server owned by the attacker must not be able to
// drive a DDNS update through a DDNS profile owned by another (victim) user.
// The worker-time resolution must skip foreign-owned profiles.
func TestGetDDNSProvidersSkipsForeignOwnedProfile(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember}, // attacker / server owner
		200: {Role: model.RoleMember}, // victim / profile owner
	})

	victimProfile := &model.DDNSProfile{
		Common:       model.Common{ID: 1, UserID: 200},
		Provider:     model.ProviderDummy,
		Name:         "victim-profile",
		AccessSecret: "victim-secret",
	}
	dc := newDDNSClassForTest(victimProfile)

	providers, err := dc.GetDDNSProvidersFromProfiles([]uint64{1}, &model.IP{}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("expected foreign-owned profile to be skipped, got %d provider(s)", len(providers))
	}
}

// A server owner using their own DDNS profile must still resolve normally.
func TestGetDDNSProvidersAllowsOwnedProfile(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember},
	})

	ownProfile := &model.DDNSProfile{
		Common:   model.Common{ID: 5, UserID: 100},
		Provider: model.ProviderDummy,
		Name:     "own-profile",
	}
	dc := newDDNSClassForTest(ownProfile)

	providers, err := dc.GetDDNSProvidersFromProfiles([]uint64{5}, &model.IP{}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("expected owned profile to resolve, got %d provider(s)", len(providers))
	}
}

// Administrative ownership is not an explicit grant to another tenant.
func TestGetDDNSProvidersSkipsForeignAdminOwnedProfile(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		1:   {Role: model.RoleAdmin}, // admin / profile owner
		100: {Role: model.RoleMember},
	})

	adminProfile := &model.DDNSProfile{
		Common:   model.Common{ID: 9, UserID: 1},
		Provider: model.ProviderDummy,
		Name:     "admin-profile",
	}
	dc := newDDNSClassForTest(adminProfile)

	providers, err := dc.GetDDNSProvidersFromProfiles([]uint64{9}, &model.IP{}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("expected foreign admin-owned profile to be skipped, got %d provider(s)", len(providers))
	}
}

// GHSA-39g2-8x68-pmx8 (UserID==0 variant): userIsAdmin(0) returns true as a
// "system resource" shortcut, but a DDNS profile with UserID==0 is not a real
// admin grant — it is a migration/default-value artifact. A foreign server
// owner must NOT be able to drive an update through such a profile, so the
// worker must skip a UserID==0 profile that the caller does not own.
func TestGetDDNSProvidersSkipsUnownedZeroUserProfile(t *testing.T) {
	replaceUserInfoMapForSecurityTest(t, map[uint64]model.UserInfo{
		100: {Role: model.RoleMember}, // attacker / server owner
	})

	orphanProfile := &model.DDNSProfile{
		Common:       model.Common{ID: 3, UserID: 0},
		Provider:     model.ProviderDummy,
		Name:         "orphan-profile",
		AccessSecret: "orphan-secret",
	}
	dc := newDDNSClassForTest(orphanProfile)

	providers, err := dc.GetDDNSProvidersFromProfiles([]uint64{3}, &model.IP{}, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(providers) != 0 {
		t.Fatalf("expected UserID==0 foreign profile to be skipped, got %d provider(s)", len(providers))
	}
}

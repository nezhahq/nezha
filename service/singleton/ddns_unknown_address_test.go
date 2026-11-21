package singleton

import (
	"testing"

	"github.com/nezhahq/nezha/model"
)

// A server's address lives in memory only. After a dashboard restart, or while
// the agent is offline, it is empty until the agent reports again. Saving the
// server in that window used to start a DDNS update with no addresses, and the
// update treats an empty address as "delete this record": the A and AAAA
// records of a healthy server were removed.
func TestUpdateDDNSSkipsServerWhoseAddressIsUnknown(t *testing.T) {
	previousDDNS, previousConf := DDNSShared, Conf
	t.Cleanup(func() { DDNSShared, Conf = previousDDNS, previousConf })
	Conf = &ConfigClass{Config: &model.Config{}}
	// The profile is deliberately missing: resolving it fails, which makes
	// "an update was attempted" observable without any network access.
	DDNSShared = newDDNSClassForTest()
	servers := &ServerClass{}

	addressNeverReported := &model.Server{Common: model.Common{ID: 1, UserID: 100}, EnableDDNS: true, DDNSProfiles: []uint64{7}, GeoIP: &model.GeoIP{}}
	if err := servers.UpdateDDNS(addressNeverReported, nil); err != nil {
		t.Errorf("update for a server without a known address was attempted: %v", err)
	}
	if err := servers.UpdateDDNS(addressNeverReported, &model.IP{}); err != nil {
		t.Errorf("update with an explicitly empty address was attempted: %v", err)
	}
	withoutGeoIP := &model.Server{Common: model.Common{ID: 2, UserID: 100}, EnableDDNS: true, DDNSProfiles: []uint64{7}}
	if err := servers.UpdateDDNS(withoutGeoIP, nil); err != nil {
		t.Errorf("update for a server without GeoIP state was attempted: %v", err)
	}

	// Control: once an address is known the update proceeds (and here fails on
	// the missing profile).
	if err := servers.UpdateDDNS(addressNeverReported, &model.IP{IPv4Addr: "192.0.2.10"}); err == nil {
		t.Error("update with a known address did not reach the DDNS profiles")
	}
}

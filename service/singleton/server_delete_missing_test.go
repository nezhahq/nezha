package singleton

import (
	"testing"

	"github.com/nezhahq/nezha/model"
)

func TestServerClassDeleteMissingIDNoPanic(t *testing.T) {
	c := &ServerClass{
		class: class[uint64, *model.Server]{
			list: map[uint64]*model.Server{
				1: {Common: model.Common{ID: 1}, UUID: "uuid-1"},
			},
		},
		uuidToID: map[string]uint64{"uuid-1": 1},
	}

	c.Delete([]uint64{999999})

	if _, ok := c.list[1]; !ok {
		t.Fatalf("existing server 1 must remain after deleting a non-existent id")
	}

	c.Delete([]uint64{1, 424242})
	if _, ok := c.list[1]; ok {
		t.Fatalf("server 1 should be removed")
	}
	if _, ok := c.uuidToID["uuid-1"]; ok {
		t.Fatalf("uuid mapping for server 1 should be removed")
	}
}

// An edit that read the server before it was deleted must not bring it back.
func TestServerClassReplaceDoesNotRestoreDeletedServer(t *testing.T) {
	server := &model.Server{Common: model.Common{ID: 1}, UUID: "uuid-1"}
	model.InitServer(server)
	c := &ServerClass{
		class: class[uint64, *model.Server]{
			list: map[uint64]*model.Server{1: server},
		},
		uuidToID: map[string]uint64{"uuid-1": 1},
	}

	if !c.Replace(&model.Server{Common: model.Common{ID: 1}, UUID: "uuid-1", Name: "edited"}) {
		t.Fatalf("editing a registered server must succeed")
	}
	edited, _ := c.Get(1)
	if edited == nil || edited.Name != "edited" {
		t.Fatalf("the edit was not published")
	}
	if edited.RuntimeHandle() != server.RuntimeHandle() {
		t.Fatalf("the edited server must keep the running server's runtime state")
	}
	if id, ok := c.UUIDToID("uuid-1"); !ok || id != 1 {
		t.Fatalf("editing server 1 must keep its UUID registered")
	}

	c.Delete([]uint64{1})
	if c.Replace(&model.Server{Common: model.Common{ID: 1}, UUID: "uuid-1"}) {
		t.Fatalf("editing a deleted server must fail")
	}
	if _, ok := c.Get(1); ok {
		t.Fatalf("deleted server 1 is registered again")
	}
}

// Deleting a stale entry must not take the UUID away from the server that has
// registered it since.
func TestServerClassDeleteKeepsUUIDOfAnotherServer(t *testing.T) {
	stale := &model.Server{Common: model.Common{ID: 1}, UUID: "uuid-1"}
	current := &model.Server{Common: model.Common{ID: 2}, UUID: "uuid-1"}
	c := &ServerClass{
		class: class[uint64, *model.Server]{
			list: map[uint64]*model.Server{1: stale, 2: current},
		},
		uuidToID: map[string]uint64{"uuid-1": 2},
	}

	c.Delete([]uint64{1})
	if id, ok := c.UUIDToID("uuid-1"); !ok || id != 2 {
		t.Fatalf("deleting server 1 unregistered the UUID of server 2")
	}

	c.Delete([]uint64{2})
	if _, ok := c.UUIDToID("uuid-1"); ok {
		t.Fatalf("uuid mapping for server 2 should be removed")
	}
}

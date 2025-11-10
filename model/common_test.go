package model

import (
	"reflect"
	"slices"
	"testing"
)

func TestSearchByID(t *testing.T) {
	t.Run("WithoutPriorityList", func(t *testing.T) {
		list, exp := []*DDNSProfile{
			{Common: Common{ID: 1}},
			{Common: Common{ID: 2}},
			{Common: Common{ID: 3}},
			{Common: Common{ID: 4}},
			{Common: Common{ID: 5}},
		}, []*DDNSProfile{
			{Common: Common{ID: 4}},
			{Common: Common{ID: 1}},
			{Common: Common{ID: 3}},
		}

		searchList := slices.Values([]string{"4", "1", "3"})
		filtered := SearchByID(searchList, list)
		if !reflect.DeepEqual(filtered, exp) {
			t.Fatalf("expected %v, but got %v", exp, filtered)
		}
	})

	t.Run("WithPriorityTest", func(t *testing.T) {
		list, exp := []*Server{
			{Common: Common{ID: 5}, DisplayIndex: 2},
			{Common: Common{ID: 4}, DisplayIndex: 1},
			{Common: Common{ID: 1}},
			{Common: Common{ID: 2}},
			{Common: Common{ID: 3}},
		}, []*Server{
			{Common: Common{ID: 4}, DisplayIndex: 1},
			{Common: Common{ID: 5}, DisplayIndex: 2},
			{Common: Common{ID: 3}},
		}

		searchList := slices.Values([]string{"3", "4", "5"})
		filtered := SearchByID(searchList, list)
		if !reflect.DeepEqual(filtered, exp) {
			t.Fatalf("expected %v, but got %v", exp, filtered)
		}
	})
}

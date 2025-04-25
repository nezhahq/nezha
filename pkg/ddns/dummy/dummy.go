package dummy

import (
	"context"

	"github.com/libdns/libdns"
)

// Internal use
type Provider struct{}

func (provider *Provider) SetRecords(ctx context.Context, zone string,
	recs []libdns.Record) ([]libdns.Record, error) {
	return recs, nil
}

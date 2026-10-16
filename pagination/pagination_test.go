package pagination_test

import (
	"net/url"
	"testing"

	"github.com/rocketinsidecorp/forge-go/pagination"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	id := uuid.MustNew()
	decoded, err := pagination.DecodeCursor(pagination.EncodeCursor(id))
	if err != nil || decoded != id {
		t.Fatalf("decoded=%s err=%v want %s", decoded, err, id)
	}
}

func TestFromQuery(t *testing.T) {
	t.Parallel()
	id := uuid.MustNew()
	request, err := pagination.FromQuery(url.Values{"cursor": {pagination.EncodeCursor(id)}, "limit": {"10"}})
	if err != nil || request.After != id || request.Limit != 10 || !request.HasCursor() {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	first, err := pagination.FromQuery(url.Values{})
	if err != nil || first.Limit != pagination.DefaultLimit || first.HasCursor() {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	for _, query := range []url.Values{
		{"limit": {"0"}}, {"limit": {"201"}}, {"limit": {"ten"}},
		{"cursor": {"not base64!"}}, {"cursor": {"AAAA"}},
		{"cursor": {pagination.EncodeCursor(uuid.UUID{})}}, // not a UUIDv7
	} {
		if _, err := pagination.FromQuery(query); err == nil {
			t.Errorf("query %v unexpectedly accepted", query)
		}
	}
}

func TestNewPage(t *testing.T) {
	t.Parallel()
	ids := []uuid.UUID{uuid.MustNew(), uuid.MustNew(), uuid.MustNew()}
	identity := func(id uuid.UUID) uuid.UUID { return id }

	page := pagination.NewPage(ids, 2, identity)
	if len(page.Items) != 2 || page.NextCursor != pagination.EncodeCursor(ids[1]) {
		t.Fatalf("page=%+v", page)
	}
	last := pagination.NewPage(ids[:2], 2, identity)
	if len(last.Items) != 2 || last.NextCursor != "" {
		t.Fatalf("last=%+v", last)
	}
	if empty := pagination.NewPage[uuid.UUID](nil, 2, identity); empty.Items == nil {
		t.Fatal("empty page must encode items as [] not null")
	}
}

func FuzzDecodeCursor(f *testing.F) {
	f.Add(pagination.EncodeCursor(uuid.MustNew()))
	f.Add("")
	f.Add("%%%")
	f.Fuzz(func(t *testing.T, cursor string) {
		id, err := pagination.DecodeCursor(cursor)
		if err == nil && (id.Version() != 7 || pagination.EncodeCursor(id) != cursor) {
			t.Fatalf("accepted non-canonical cursor %q", cursor)
		}
	})
}

package plex

import (
	"encoding/json"
	"testing"
)

// Plex sends both "guid" (a single string) and "Guid" (an array of
// external ids). Go matches JSON keys to struct fields
// case-insensitively when there is no exact match, so both land in the
// same field. Decoding used to fail outright on the string, which is
// what broke the first real catalogue build.
func TestGuidAcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{{
		name: "array only",
		body: `{"title":"Dune","Guid":[{"id":"imdb://tt1160419"},{"id":"tmdb://438631"}]}`,
		want: "438631",
	}, {
		name: "string only",
		body: `{"title":"Dune","guid":"plex://movie/5d776be07a53e9001e732ab9"}`,
		want: "",
	}, {
		// The string arriving after the array must not wipe it. The two
		// keys can come in either order.
		name: "string after array",
		body: `{"title":"Dune","Guid":[{"id":"tmdb://438631"}],"guid":"plex://movie/5d77"}`,
		want: "438631",
	}, {
		name: "string before array",
		body: `{"title":"Dune","guid":"plex://movie/5d77","Guid":[{"id":"tmdb://438631"}]}`,
		want: "438631",
	}, {
		name: "absent",
		body: `{"title":"Dune"}`,
		want: "",
	}, {
		name: "null",
		body: `{"title":"Dune","Guid":null}`,
		want: "",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var it Item
			if err := json.Unmarshal([]byte(c.body), &it); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := it.ExternalID("tmdb"); got != c.want {
				t.Errorf("tmdb id = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExternalIDPicksTheRightScheme(t *testing.T) {
	it := Item{Guid: GuidList{{ID: "imdb://tt0133093"}, {ID: "tmdb://603"}, {ID: "tvdb://1234"}}}
	for scheme, want := range map[string]string{
		"imdb": "tt0133093",
		"tmdb": "603",
		"tvdb": "1234",
		"nope": "",
	} {
		if got := it.ExternalID(scheme); got != want {
			t.Errorf("ExternalID(%q) = %q, want %q", scheme, got, want)
		}
	}
}

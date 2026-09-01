package gmaps

import (
	"encoding/json"
	"testing"
)

// state builds an APP_INITIALIZATION_STATE blob carrying just the two numbers the completeness
// check reads: the rating at darray[4][7] and the review count at darray[4][8].
func state(rating float64, count int) []byte {
	sub := make([]any, 9)
	sub[7] = rating
	sub[8] = float64(count)

	darray := make([]any, 5)
	darray[4] = sub

	jd := make([]any, 7)
	jd[6] = darray

	raw, err := json.Marshal(jd)
	if err != nil {
		panic(err)
	}

	return raw
}

func TestHasCompletePlaceData(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		raw    []byte
		want   bool
		reason string
	}{
		{
			name:   "rating without a count is a page still hydrating",
			raw:    state(4.5, 0),
			want:   false,
			reason: "Google never shows a rating for a place with no reviews, so the pair is impossible",
		},
		{
			name: "a count present means the detail fields have landed",
			raw:  state(4.5, 1509),
			want: true,
		},
		{
			name: "no rating and no count is a genuine unreviewed place, not a stall",
			raw:  state(0, 0),
			want: true,
		},
		{
			name: "unparseable input is never treated as complete",
			raw:  []byte("not json"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := hasCompletePlaceData(tc.raw); got != tc.want {
				t.Fatalf("hasCompletePlaceData() = %v, want %v (%s)", got, tc.want, tc.reason)
			}
		})
	}
}

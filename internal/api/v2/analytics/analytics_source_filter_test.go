// analytics_source_filter_test.go: tests for the source_id query parameter on /analytics/*.
package analytics

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/birdnet-go/internal/datastore"
)

// TestParseOptionalSourceIDs covers the input parsing layer in isolation.
// The handler tests below cover end-to-end pass-through into the datastore call.
func TestParseOptionalSourceIDs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		want  []uint
	}{
		{name: "absent param returns nil", query: "", want: nil},
		{name: "single id", query: "?source_id=3", want: []uint{3}},
		{name: "comma-separated list preserves order", query: "?source_id=2,5,1", want: []uint{2, 5, 1}},
		{name: "whitespace tolerated", query: "?source_id=%202%20,%205%20,%20%201%20", want: []uint{2, 5, 1}},
		{name: "duplicates collapsed", query: "?source_id=1,1,2,2,1", want: []uint{1, 2}},
		{name: "zero rejected", query: "?source_id=0", want: nil},
		{name: "negative rejected, valid kept", query: "?source_id=-1,4", want: []uint{4}},
		{name: "non-numeric rejected, valid kept", query: "?source_id=foo,7", want: []uint{7}},
		{name: "empty tokens skipped", query: "?source_id=,1,,3,", want: []uint{1, 3}},
		{name: "all invalid returns nil", query: "?source_id=foo,bar,0", want: nil},
		{name: "value with only whitespace returns nil", query: "?source_id=%20%20%20", want: nil},
	}

	_, _, c := setupAnalyticsTestEnvironment(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/x"+tc.query, http.NoBody)
			rec := httptest.NewRecorder()
			ctx := e.NewContext(req, rec)
			got := c.parseOptionalSourceIDs(ctx, "source_id")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseOptionalSourceIDs_Truncation ensures the maxSourceIDsPerRequest cap is enforced;
// supplying more than the cap returns exactly the cap's worth of leading valid IDs.
// Documented in utils.go as a defensive bound on the IN-clause size.
func TestParseOptionalSourceIDs_Truncation(t *testing.T) {
	t.Parallel()

	const overflow = maxSourceIDsPerRequest + 5

	// Build "1,2,3,...,N" where N > cap.
	var b strings.Builder
	for i := 1; i <= overflow; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i))
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/x?source_id="+b.String(), http.NoBody)
	rec := httptest.NewRecorder()
	ctx := e.NewContext(req, rec)

	_, _, c := setupAnalyticsTestEnvironment(t)
	got := c.parseOptionalSourceIDs(ctx, "source_id")
	require.Len(t, got, maxSourceIDsPerRequest)
	assert.Equal(t, uint(1), got[0], "leading IDs are preserved")
	assert.Equal(t, uint(maxSourceIDsPerRequest), got[len(got)-1])
}

// TestGetSpeciesSummary_PassesSourceIDsToDatastore verifies the API handler threads the
// parsed source_id values into the datastore call as a variadic argument. Each test case
// pre-expands tc.wantSources into mockery's variadic interface{} slots — mockery treats a
// single mock.Anything as exactly one variadic value, so a generic "anything" match would
// only validate the single-source case. The pre-expansion gives us per-slot value matching
// (so the test fails loudly on misordered or dropped IDs) plus a .Once() assertion that
// the call happened exactly the way the handler should have made it.
func TestGetSpeciesSummary_PassesSourceIDsToDatastore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		query       string
		wantSources []uint
	}{
		{name: "no source_id passes empty variadic", query: "?start_date=2026-01-01&end_date=2026-01-31", wantSources: nil},
		{name: "single source_id forwarded", query: "?start_date=2026-01-01&end_date=2026-01-31&source_id=7", wantSources: []uint{7}},
		{name: "comma list forwarded preserving order", query: "?start_date=2026-01-01&end_date=2026-01-31&source_id=11,22,33", wantSources: []uint{11, 22, 33}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, mockDS, controller := setupAnalyticsTestEnvironment(t)

			variadicMatchers := make([]any, 0, len(tc.wantSources))
			for _, id := range tc.wantSources {
				variadicMatchers = append(variadicMatchers, id)
			}
			mockDS.EXPECT().
				GetSpeciesSummaryData(mock.Anything, "2026-01-01", "2026-01-31", variadicMatchers...).
				Return([]datastore.SpeciesSummaryData{}, nil).
				Once()

			req := httptest.NewRequest(http.MethodGet, "/api/v2/analytics/species/summary"+tc.query, http.NoBody)
			rec := httptest.NewRecorder()
			ctx := e.NewContext(req, rec)

			err := controller.GetSpeciesSummary(ctx)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

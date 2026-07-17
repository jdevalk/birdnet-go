package detections

import (
	"testing"

	"gorm.io/gorm"

	"github.com/tphakala/birdnet-go/internal/api/v2/apicore"
	"github.com/tphakala/birdnet-go/internal/datastore"
	v2 "github.com/tphakala/birdnet-go/internal/datastore/v2"
)

// stubManager is a v2.Manager whose only meaningful behaviour is the table
// prefix; every other method is a no-op, since v2TablePrefix consults nothing
// else.
type stubManager struct {
	prefix string
}

func (s *stubManager) Initialize() error    { return nil }
func (s *stubManager) DB() *gorm.DB         { return nil }
func (s *stubManager) Path() string         { return "" }
func (s *stubManager) Close() error         { return nil }
func (s *stubManager) CheckpointWAL() error { return nil }
func (s *stubManager) Delete() error        { return nil }
func (s *stubManager) Exists() bool         { return true }
func (s *stubManager) IsMySQL() bool        { return true }
func (s *stubManager) TablePrefix() string  { return s.prefix }

// managerDS is a datastore.Interface that also exposes a v2 manager, mirroring
// *v2only.Datastore. Only Manager() is exercised here; the embedded interface
// is nil because v2TablePrefix never calls through it.
type managerDS struct {
	datastore.Interface
	mgr v2.Manager
}

func (m *managerDS) Manager() v2.Manager { return m.mgr }

// plainDS is a datastore.Interface with no Manager(), like the legacy store.
type plainDS struct {
	datastore.Interface
}

// TestV2TablePrefix covers the resolution that keeps this endpoint's raw SQL
// pointed at the right tables. A MySQL install inside the v1→v2 migration
// window prefixes every v2 table with "v2_", so hardcoding bare names compiles
// and passes the SQLite-backed suite (prefix "") while failing in production
// with `Table 'birdnet.ai_models' doesn't exist`.
func TestV2TablePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ds   datastore.Interface
		want string
	}{
		{
			name: "migration-window MySQL reports the v2_ prefix",
			ds:   &managerDS{mgr: &stubManager{prefix: "v2_"}},
			want: "v2_",
		},
		{
			name: "fresh install reports no prefix",
			ds:   &managerDS{mgr: &stubManager{prefix: ""}},
			want: "",
		},
		{
			name: "legacy store without a manager falls back to no prefix",
			ds:   &plainDS{},
			want: "",
		},
		{
			name: "nil manager falls back to no prefix",
			ds:   &managerDS{mgr: nil},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &Handler{Core: &apicore.Core{DS: tt.ds}}
			if got := c.v2TablePrefix(); got != tt.want {
				t.Errorf("v2TablePrefix() = %q, want %q", got, tt.want)
			}
		})
	}
}

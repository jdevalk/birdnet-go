// internal/api/v2/correct_species.go
//
// Operator-driven correction of a detection's species. After running the
// reanalyze endpoint and seeing what other models think, the user can pick
// "this is the right one" — this endpoint updates the detection record in
// place to reflect that choice, and marks it as verified='correct' in the
// same step.
//
// Implementation note: the correction is written via raw SQL against the v2
// schema (detections + labels + ai_models tables) rather than through the
// datastore.Interface, because v2only doesn't expose a generic UpdateNote
// surface and adding one would require regenerating mockery mocks for a
// single-shot endpoint. The v2 schema is stable; the raw SQL is scoped to
// three statements inside a single transaction.
//
// Because these statements name tables directly, every one of them must carry
// the deployment's v2 table prefix (see v2TablePrefix) — it is "v2_" on a
// MySQL install still inside the v1→v2 migration window and "" elsewhere.
// Hardcoding the unprefixed names compiles and passes the SQLite-backed tests,
// where the prefix is "", while failing at runtime on exactly the MySQL
// deployments this endpoint is used on.
package detections

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tphakala/birdnet-go/internal/classifier"
	v2 "github.com/tphakala/birdnet-go/internal/datastore/v2"
	"github.com/tphakala/birdnet-go/internal/logger"
)

// v2ManagerProvider is implemented by *v2only.Datastore, the store this
// endpoint's raw SQL targets. It is asserted rather than added to
// datastore.Interface so the legacy store (which has no v2 schema, and so
// never reaches these statements) needs no changes.
type v2ManagerProvider interface {
	Manager() v2.Manager
}

// v2TablePrefix returns the prefix that raw-SQL references to v2 tables must
// carry. It is "v2_" only on MySQL deployments still inside the v1→v2
// migration window, where the v2 tables coexist with the legacy schema, and ""
// on SQLite and fresh-install MySQL — see Manager.TablePrefix in
// internal/datastore/v2/manager.go, which requires raw SQL to consult it.
// Falls back to "" when the prefix cannot be resolved, matching the unprefixed
// default rather than inventing a prefix that may not exist.
func (c *Handler) v2TablePrefix() string {
	if p, ok := c.DS.(v2ManagerProvider); ok {
		if m := p.Manager(); m != nil {
			return m.TablePrefix()
		}
	}
	return ""
}

// CorrectSpeciesRequest is the JSON body of
// POST /api/v2/detections/:id/correct-species.
type CorrectSpeciesRequest struct {
	// ScientificName is the binomial Latin name the user is asserting (e.g.
	// "Ficedula hypoleuca"). Must match an existing labels row keyed by the
	// chosen ModelID — we never create labels here, because doing so would
	// fork the model's vocabulary.
	ScientificName string `json:"scientificName"`

	// ModelID is the orchestrator registry ID (e.g. "BirdNET_V2.4",
	// "Perch_V2") or the user-facing config alias (e.g. "birdnet"). The
	// resulting detection record carries this model's ID and the label_id
	// from that model's vocabulary. Required.
	ModelID string `json:"modelId"`

	// Confidence is the confidence value to record on the corrected
	// detection. Typically the max confidence the chosen model produced for
	// this species in the reanalysis pass. Range [0, 1].
	Confidence float64 `json:"confidence"`
}

// CorrectSpeciesResponse describes the corrected detection.
type CorrectSpeciesResponse struct {
	DetectionID    uint    `json:"detectionId"`
	ScientificName string  `json:"scientificName"`
	CommonName     string  `json:"commonName"`
	ModelID        string  `json:"modelId"`
	ModelName      string  `json:"modelName"`
	Confidence     float64 `json:"confidence"`
	Verified       string  `json:"verified"`
}

// CorrectDetectionSpecies handles POST /api/v2/detections/:id/correct-species.
// @Summary Correct a detection's species and mark it verified
// @Description Replaces the detection's species/confidence/model with the
// @Description chosen prediction (typically from a reanalyze response), and
// @Description records a 'correct' review in one transaction. The detection
// @Description must be unlocked.
// @Tags detections
// @Accept json
// @Produce json
// @Param id path int true "Detection ID"
// @Param request body CorrectSpeciesRequest true "Correction payload"
// @Success 200 {object} CorrectSpeciesResponse "Updated detection state"
// @Failure 400 {object} ErrorResponse "Invalid request or unknown species/model"
// @Failure 404 {object} ErrorResponse "Detection not found"
// @Failure 409 {object} ErrorResponse "Detection is locked"
// @Failure 500 {object} ErrorResponse "Database failure"
// @Router /detections/{id}/correct-species [post]
func (c *Handler) CorrectDetectionSpecies(ctx echo.Context) error {
	idStr := ctx.Param("id")
	noteIDUint, parseErr := strconv.ParseUint(idStr, 10, 64)
	if parseErr != nil {
		return c.HandleError(ctx, parseErr,
			"Detection ID must be a numeric value", http.StatusBadRequest)
	}

	req := &CorrectSpeciesRequest{}
	if err := ctx.Bind(req); err != nil {
		return c.HandleError(ctx, err, "Invalid request body", http.StatusBadRequest)
	}
	if req.ScientificName == "" {
		return c.HandleError(ctx,
			fmt.Errorf("scientificName is required"),
			"scientificName is required", http.StatusBadRequest)
	}
	if req.ModelID == "" {
		return c.HandleError(ctx,
			fmt.Errorf("modelId is required"),
			"modelId is required", http.StatusBadRequest)
	}
	if req.Confidence < 0 || req.Confidence > 1 {
		return c.HandleError(ctx,
			fmt.Errorf("confidence %.4f out of range", req.Confidence),
			"confidence must be in [0, 1]", http.StatusBadRequest)
	}

	// Verify the detection exists and capture its current state so we can
	// log the before/after change. Lock state is checked atomically inside
	// the transaction below.
	existing, err := c.DS.Get(idStr)
	if err != nil {
		return c.HandleError(ctx, err, "Detection not found", http.StatusNotFound)
	}
	if existing.Locked {
		return c.HandleError(ctx,
			fmt.Errorf("detection is locked"),
			"Detection is locked and cannot be corrected; unlock it first",
			http.StatusConflict)
	}

	// Resolve the orchestrator registry ID through the alias chain. We need
	// the orchestrator's ModelInfo to translate registry ID → (name,
	// version) so we can map to the ai_models row, and to confirm the
	// model is actually loaded (we won't accept corrections naming a model
	// the running server doesn't know about).
	bn, err := c.GetBirdNETInstance()
	if err != nil {
		return c.HandleError(ctx, err,
			"Classifier not available", http.StatusServiceUnavailable)
	}
	resolvedID := req.ModelID
	if registryID, ok := classifier.ResolveConfigModelID(req.ModelID); ok {
		resolvedID = registryID
	}
	// Index-based loop so we don't copy classifier.ModelInfo (~224 bytes)
	// every iteration just to read .ID. Only the three string fields we
	// need are pulled out into locals.
	var modelName, modelVersion, modelDisplayName string
	infos := bn.ModelInfos()
	for i := range infos {
		if infos[i].ID == resolvedID {
			modelName = infos[i].DetectionName
			modelVersion = infos[i].DetectionVersion
			modelDisplayName = infos[i].Name
			break
		}
	}
	if modelName == "" || modelVersion == "" {
		return c.HandleError(ctx,
			fmt.Errorf("model %q is not loaded", req.ModelID),
			"Specified model is not loaded; cannot apply correction",
			http.StatusBadRequest)
	}

	// Single-transaction correction: look up the model and label rows, update
	// the detection, and write the review record. If any step fails the whole
	// thing rolls back so the operator never sees a half-applied state.
	// Uses datastore.Interface.Transaction so the call works against the
	// v2only adapter without reaching into the concrete *DataStore.
	var (
		newModelID uint
		newLabelID uint
	)
	// Raw-SQL table references must carry the deployment's v2 prefix; see
	// v2TablePrefix. Resolved once so every statement below agrees.
	prefix := c.v2TablePrefix()

	correctionErr := c.DS.Transaction(func(tx *gorm.DB) error {
		// 1. Map orchestrator (name, version) → ai_models.id
		if err := tx.Table(prefix+"ai_models").
			Select("id").
			Where("name = ? AND version = ?", modelName, modelVersion).
			Scan(&newModelID).Error; err != nil {
			return fmt.Errorf("ai_models lookup failed: %w", err)
		}
		if newModelID == 0 {
			return fmt.Errorf("model %s/%s not present in ai_models table", modelName, modelVersion)
		}

		// 2. Find label_id for (scientific_name, model_id). We do NOT create
		// a new label here — if the chosen model doesn't have a label for
		// this species, the user must pick a different model.
		if err := tx.Table(prefix+"labels").
			Select("id").
			Where("scientific_name = ? AND model_id = ?", req.ScientificName, newModelID).
			Scan(&newLabelID).Error; err != nil {
			return fmt.Errorf("labels lookup failed: %w", err)
		}
		if newLabelID == 0 {
			return fmt.Errorf("species %q not in %s's vocabulary", req.ScientificName, modelDisplayName)
		}

		// 3. Update detection. Locked check is duplicated here in case the
		// lock was set between our Get() above and now (TOCTOU). Using
		// raw_value() COALESCE pattern would also work; explicit
		// NOT EXISTS keeps it readable.
		result := tx.Exec(fmt.Sprintf(
			`UPDATE %sdetections
			    SET label_id = ?, model_id = ?, confidence = ?
			  WHERE id = ?
			    AND NOT EXISTS (SELECT 1 FROM %sdetection_locks WHERE detection_id = ?)`,
			prefix, prefix),
			newLabelID, newModelID, req.Confidence, noteIDUint, noteIDUint)
		if result.Error != nil {
			return fmt.Errorf("detection update failed: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("detection %d was locked or does not exist", noteIDUint)
		}

		// 4. Upsert the review row, keyed by the unique index on detection_id.
		// Built through clause.OnConflict rather than hand-written upsert SQL:
		// "ON CONFLICT ... DO UPDATE" is SQLite/Postgres-only, and MySQL needs
		// "ON DUPLICATE KEY UPDATE". GORM emits the right dialect for both, so
		// this works on every supported backend.
		now := tx.NowFunc()
		if err := tx.Table(prefix + "detection_reviews").
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "detection_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"verified", "updated_at"}),
			}).
			Create(map[string]any{
				"detection_id": noteIDUint,
				"verified":     "correct",
				"created_at":   now,
				"updated_at":   now,
			}).Error; err != nil {
			return fmt.Errorf("review upsert failed: %w", err)
		}

		return nil
	})

	if correctionErr != nil {
		c.LogAPIRequest(ctx, logger.LogLevelError, "Species correction failed",
			logger.String("detection_id", idStr),
			logger.String("model_id", req.ModelID),
			logger.String("scientific_name", req.ScientificName),
			logger.Error(correctionErr))
		status := http.StatusInternalServerError
		// Map known-cause errors to clearer status codes for the UI.
		if isCorrectionUserError(correctionErr) {
			status = http.StatusBadRequest
		}
		return c.HandleError(ctx, correctionErr,
			fmt.Sprintf("Failed to correct species: %s", correctionErr.Error()),
			status)
	}

	// Resolve a locale-appropriate common name for the response. The
	// resolver chain on the orchestrator covers both BirdNET's labels and
	// the v3 geomodel taxonomy CSV companion (PR #3042 upstream), so this
	// works for any species in either model's vocabulary in 24+ locales.
	common := bn.ResolveName(req.ScientificName, c.CurrentSettings().BirdNET.Locale)

	// Invalidate the detection-list cache so dashboards/species pages reflect
	// the new label immediately. Without this, the 5-minute species-detection
	// cache continues serving the pre-correction species name (and stale
	// confidence/verified state) for any list query that included this
	// detection — the operator sees the correction stick on the detail page
	// but the parent list still shows the old label for up to 5 minutes.
	// Same pattern Delete, Review, and Lock handlers use.
	c.invalidateDetectionCache()

	c.LogAPIRequest(ctx, logger.LogLevelInfo, "Species correction applied",
		logger.String("detection_id", idStr),
		logger.String("model_id", resolvedID),
		logger.String("from_species", existing.ScientificName),
		logger.String("to_species", req.ScientificName),
		logger.Float64("from_confidence", existing.Confidence),
		logger.Float64("to_confidence", req.Confidence))

	return ctx.JSON(http.StatusOK, CorrectSpeciesResponse{
		DetectionID:    existing.ID,
		ScientificName: req.ScientificName,
		CommonName:     common,
		ModelID:        resolvedID,
		ModelName:      modelDisplayName,
		Confidence:     req.Confidence,
		Verified:       "correct",
	})
}

// isCorrectionUserError reports whether err describes a problem with the
// caller's input (unknown model, unknown species, lock conflict) vs an
// internal DB failure. Used to map error → HTTP status.
func isCorrectionUserError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// The transaction body returns wrapped fmt.Errorf strings; pattern match
	// here is the only way to distinguish since we don't define typed errors
	// for these conditions inline. Keep messages stable when editing.
	for _, marker := range []string{
		"not in",
		"is locked",
		"does not exist",
		"not present in ai_models",
		"is not loaded",
	} {
		if containsCaseInsensitive(msg, marker) {
			return true
		}
	}
	return false
}

// containsCaseInsensitive is a tiny helper; stdlib strings.Contains is
// case-sensitive and pulling in a regex for one substring check would be
// overkill.
func containsCaseInsensitive(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	// ASCII fast path — all markers above are ASCII.
	for i := 0; i+len(substr) <= len(s); i++ {
		match := true
		for j := range len(substr) {
			a := s[i+j]
			b := substr[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

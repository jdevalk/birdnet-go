// clip_path.go holds the fork-local clip-path glue used by the reanalyze and
// species-correction handlers. Both need to turn a datastore clip path into a
// validated SecureFS-relative path before decoding audio.
//
// Upstream keeps equivalent helpers in the media domain package, but they are
// unexported there and reanalyze/correct-species are fork-local features that
// do not otherwise depend on media. Rather than duplicate the path-security
// logic (which would silently miss upstream hardening fixes), this file is thin
// glue over the same shared primitives the media domain uses:
// apicore.NormalizeClipPath for prefix normalization and SecureFS.ValidateRelativePath
// for traversal checks. Keeping it fork-local means upstream refactors of the
// media package do not conflict with these features.
package detections

import (
	"os"

	"gorm.io/gorm"

	"github.com/tphakala/birdnet-go/internal/api/v2/apicore"
	"github.com/tphakala/birdnet-go/internal/datastore/v2/repository"
	"github.com/tphakala/birdnet-go/internal/errors"
	"github.com/tphakala/birdnet-go/internal/logger"
	"github.com/tphakala/birdnet-go/internal/securefs"
)

var (
	// ErrInvalidClipPath marks a clip path that normalizes to nothing or fails
	// SecureFS validation.
	ErrInvalidClipPath = errors.NewStd("invalid clip path")
	// ErrClipPathTraversal marks a clip path that attempts to escape the
	// SecureFS sandbox.
	ErrClipPathTraversal = errors.NewStd("security error: clip path attempts to traverse")
)

// isClipNotFoundErr reports whether err indicates the audio clip or its parent
// detection does not exist. It checks the sentinel errors from the v2 repository
// layer, GORM's record-not-found, and the standard os.ErrNotExist. Mirrors the
// media domain's check for the fork-local reanalyze path.
func isClipNotFoundErr(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, gorm.ErrRecordNotFound) ||
		errors.Is(err, repository.ErrDetectionNotFound) ||
		errors.Is(err, repository.ErrNoClipPath)
}

// normalizeAndValidateClipPath normalizes a stored clip path against the configured
// clips prefix and validates it against the SecureFS sandbox, returning the
// sandbox-relative path. log may be nil.
func (c *Handler) normalizeAndValidateClipPath(audioPath string, log logger.Logger) (string, error) {
	clipsPrefix := c.CurrentSettings().Realtime.Audio.Export.Path
	normalizedPath := apicore.NormalizeClipPath(audioPath, clipsPrefix)

	if log != nil && normalizedPath != audioPath {
		log.Debug("Normalized clip path",
			logger.String("original_path", audioPath),
			logger.String("normalized_path", normalizedPath),
			logger.String("clips_prefix", clipsPrefix))
	}

	if normalizedPath == "" {
		if log != nil {
			log.Warn("Invalid clip path detected",
				logger.String("original_path", audioPath),
				logger.String("clips_prefix", clipsPrefix))
		}
		return "", errors.Newf("%w: empty normalized path", ErrInvalidClipPath).Build()
	}

	relAudioPath, err := c.SFS.ValidateRelativePath(normalizedPath)
	if err != nil {
		if errors.Is(err, securefs.ErrPathTraversal) {
			return "", errors.Newf("%w: %w", ErrClipPathTraversal, err).Build()
		}
		return "", errors.Newf("%w: %w", ErrInvalidClipPath, err).Build()
	}

	return relAudioPath, nil
}

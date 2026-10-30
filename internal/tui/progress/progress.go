// Package progress wraps bubbles/progress with Mazarbul theming.
package progress

import (
	"charm.land/bubbles/v2/progress"
)

// New returns a progress.Model with defaults. The color gradient is the
// bubbles default for now; replace with theme-aware gradient once the
// theme palette API stabilizes.
func New() progress.Model {
	return progress.New(progress.WithDefaultBlend())
}

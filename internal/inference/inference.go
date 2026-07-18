package inference

import "context"

// TextGenerator is the narrow interface used by maintenance features. It
// intentionally has no dependency on tools or chat message persistence.
type TextGenerator interface {
	Complete(context.Context, string, string) (string, error)
}

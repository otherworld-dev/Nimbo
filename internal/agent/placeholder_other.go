//go:build !windows

package agent

import "os"

// isDehydratedPlaceholder always reports false outside Windows: the placeholder
// attributes it looks for are a Windows filesystem concept, and no supported
// non-Windows provider leaves hollow files behind for a clone to trip over.
func isDehydratedPlaceholder(os.FileInfo) bool { return false }

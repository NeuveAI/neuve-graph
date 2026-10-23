//go:build windows

package gitutil

import "context"

// Windows scans take the Git route, which remembers nothing, so claims are
// never contended there.
type answerClaim struct{}

func (s *TreeScanAnswers) tryClaim(string) (*answerClaim, bool) { return nil, false }

func (s *TreeScanAnswers) awaitClaim(context.Context, string) (bool, error) { return false, nil }

func (c *answerClaim) release() {}

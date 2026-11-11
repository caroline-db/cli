package aitools

import "testing"

// TestMain pins callerIsAgent to a human caller: the test binary inherits the
// environment of whatever runs it, and an agent (CLAUDECODE, AI_AGENT, ...)
// would otherwise disable every prompt the interactive tests drive. Tests that
// need an agent caller override it themselves.
func TestMain(m *testing.M) {
	callerIsAgent = func() bool { return false }
	m.Run()
}

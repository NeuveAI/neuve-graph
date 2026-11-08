package cli

// Settings of the shared Graph service. They are part of the environment that
// names a service, so changing one starts another.
const (
	envService            = "ENTIRE_GRAPH_SERVICE"             // "auto" forwards eligible commands; "require" never falls back
	envServiceIdle        = "ENTIRE_GRAPH_SERVICE_IDLE"        // seconds before an idle service exits (60)
	envServiceConcurrency = "ENTIRE_GRAPH_SERVICE_CONCURRENCY" // reads run at once (2)
	envServiceSnapshots   = "ENTIRE_GRAPH_SERVICE_SNAPSHOTS"   // decoded snapshots kept (2)

	// A call's class and deadline, set per call; they order the service's
	// queue and never change an answer, so they name no service.
	envClass      = "ENTIRE_GRAPH_CLASS"       // "interactive" or "background" (default)
	envDeadlineMS = "ENTIRE_GRAPH_DEADLINE_MS" // milliseconds from now the call waits for the service at most
)

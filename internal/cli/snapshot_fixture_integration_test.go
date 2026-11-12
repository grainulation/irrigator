//go:build integration

package cli

// SNAPSHOT TEST FIXTURE — not production code and not a runtime API.
//
// The HTTP/SSE panel and its in-process livePanel aggregator were removed, so no
// production code builds a combined "panel snapshot" any more. The types and the
// builder below exist only so that schema assertions for the retained shared
// projection functions (projectCost, projectLifecycles, projectCapacity,
// projectTopology, projectDelivery, panelEvents) can keep asserting one
// combined JSON shape.
// Passing a test that goes through livePanelSnapshot proves those project*
// functions behave as asserted; it does NOT prove any runtime endpoint exists.

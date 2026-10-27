// Package livetest holds the tests that run scd's outbounds against a real
// Xray 26.10.21 process (build tag xraylive, binary in SCD_XRAY_BIN). Without
// the tag the package is empty; with the tag and without SCD_XRAY_BIN every
// test is skipped.
package livetest

package platform

// The embedded manifest is a mechanical copy of the reviewed one under
// build/. Copying it by hand is how the two drift, so it is generated
// and a test asserts byte equality — the embedded copy is what runs, so
// the review has to have applied to exactly those bytes.
//
//go:generate cp ../../build/platform.lock.json platform.lock.json

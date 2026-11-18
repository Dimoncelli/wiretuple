package benchmarks

// SampleCount is the number of timed batches per scenario. Tests running under
// the race detector lower it, because the detector slows every transform.
var SampleCount = 100

// BatchSize is the number of transforms timed per sample; timing a batch keeps
// the Windows clock tick from rounding every sample up to one millisecond.
const BatchSize = 25

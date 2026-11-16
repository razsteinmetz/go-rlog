module github.com/razsteinmetz/go-rlog

go 1.25

require (
	go.yaml.in/yaml/v3 v3.0.5
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
)

// Early releases with bugs fixed in v0.3.1 (a crash on nil error values,
// silently lost logs on a bad file path, out-of-order repeat summaries),
// and without a license. Use v0.3.1 or later.
retract [v0.1.0, v0.3.0]

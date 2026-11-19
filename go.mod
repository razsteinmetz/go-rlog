module github.com/razsteinmetz/go-rlog

go 1.25

require gopkg.in/natefinch/lumberjack.v2 v2.2.1

require go.yaml.in/yaml/v4 v4.0.0

// Early releases with bugs fixed in v0.3.1 (a crash on nil error values,
// silently lost logs on a bad file path, out-of-order repeat summaries),
// and without a license. Use v0.3.1 or later.
retract [v0.1.0, v0.3.0]

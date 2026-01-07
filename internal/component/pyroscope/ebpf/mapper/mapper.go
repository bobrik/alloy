package mapper

import (
	"go.opentelemetry.io/ebpf-profiler/libpf"
	"go.opentelemetry.io/ebpf-profiler/reporter"
)

type ExecutableReporter struct {
	paths map[libpf.FileID]string
}

func NewExecutableReporter() *ExecutableReporter {
	return &ExecutableReporter{paths: map[libpf.FileID]string{}}
}

func (t *ExecutableReporter) ResolvePath(file libpf.FileID) string {
	return t.paths[file]
}

func (t *ExecutableReporter) ReportExecutable(args *reporter.ExecutableMetadata) {
	t.paths[args.MappingFile.Value().FileID] = args.Mapping.Path.String()
}

package tracer

import "fmt"

// EmbeddedProgram 描述一个预编译 eBPF 程序（gzip+base64 编码）。
type EmbeddedProgram struct {
	MinKernel string
	Flags     string
	Program   []byte
}

// embeddedPrograms 由后续脚本生成并覆盖。
// key: GOARCH (amd64/arm64)
var embeddedPrograms = map[string][]EmbeddedProgram{}

func getEmbeddedEBPFProgram(arch string) ([]byte, error) {
	progs, ok := embeddedPrograms[arch]
	if !ok || len(progs) == 0 {
		return nil, fmt.Errorf("no embedded eBPF program for arch=%s", arch)
	}

	// 当前先取第一条；后续可按内核版本做最佳匹配。
	if len(progs[0].Program) == 0 {
		return nil, fmt.Errorf("embedded eBPF program for arch=%s is empty", arch)
	}
	return progs[0].Program, nil
}

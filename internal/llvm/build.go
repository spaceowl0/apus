package llvm

// BuildOptions describe how to invoke clang on an emitted module.
type BuildOptions struct {
	Target       string // LLVM target triple, empty for the host
	OptLevel     string // 0,1,2,3,s,z
	Freestanding bool   // no libc: raw syscalls and custom _start
	LLD          bool   // link with lld (required to cross-link)
	Out          string
}

// ClangArgs returns the clang arguments (excluding the input files).
func ClangArgs(o BuildOptions) []string {
	if o.OptLevel == "" {
		o.OptLevel = "2"
	}
	args := []string{"-O" + o.OptLevel, "-Wno-override-module", "-o", o.Out}
	if o.Target != "" {
		args = append(args, "--target="+o.Target)
	}
	if o.Freestanding {
		args = append(args, "-DAPUS_FREESTANDING", "-ffreestanding", "-fno-builtin", "-nostdlib", "-static",
			"-ffunction-sections", "-fdata-sections", "-fno-stack-protector", "-fno-asynchronous-unwind-tables", "-fno-unwind-tables", "-fno-pic", "-fno-pie",
			"-Wl,--gc-sections", "-Wl,-z,norelro", "-Wl,--build-id=none", "-Wl,-N", "-Wl,-z,noseparate-code",
			"-Wl,--no-eh-frame-hdr", "-s")
	}
	if o.LLD {
		args = append(args, "-fuse-ld=lld")
	}
	return args
}

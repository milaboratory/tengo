package stdlib

import (
	"github.com/d5/tengo/v2"
)

// BuiltinModules are builtin type standard library modules.
var BuiltinModules = map[string]map[string]tengo.Object{
	"math":   mathModule,
	"os":     osModule,
	"text":   textModule,
	"times":  timesModule,
	"rand":   randModule,
	"fmt":    fmtModule,
	"json":   jsonModule,
	"base64": base64Module,
	"hex":    hexModule,
}

func init() {
	// none of the module functions keep a reference to their args slice, so
	// the VM may pass them a window of its operand stack
	for _, mod := range BuiltinModules {
		for _, obj := range mod {
			if fn, ok := obj.(*tengo.UserFunction); ok {
				fn.StackArgs = true
			}
		}
	}
}

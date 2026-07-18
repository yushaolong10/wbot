//go:build wails && darwin && cgo

package main

/*
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>

void SetWbotApplicationIcon(const void *bytes, int length);
*/
import "C"

import (
	_ "embed"
	"unsafe"
)

//go:embed assets/appicon.png
var applicationIconPNG []byte

func setApplicationIcon() {
	if len(applicationIconPNG) == 0 {
		return
	}
	C.SetWbotApplicationIcon(unsafe.Pointer(&applicationIconPNG[0]), C.int(len(applicationIconPNG)))
}

//go:build !wails

package main

import "fmt"

func main() {
	fmt.Println("Build the native Wails desktop with: make desktop-native")
}

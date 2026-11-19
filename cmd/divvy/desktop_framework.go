//go:build darwin

// Link the frameworks the Wails shell needs that the default Go cgo flags do
// not include (UTType lives in UniformTypeIdentifiers, macOS 11+).
package main

// #cgo LDFLAGS: -framework UniformTypeIdentifiers
import "C"

//go:build darwin

package main

import _ "embed"

var (
	//go:embed assets/toolbar_connected.png
	toolbarConnectedIcon []byte

	//go:embed assets/toolbar_disconnected.png
	toolbarDisconnectedIcon []byte
)

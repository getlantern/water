module github.com/refraction-networking/water

go 1.24.0

retract (
	v0.6.1 // bad submodule
	v0.6.0 // bad LICENSE naming
)

replace github.com/tetratelabs/wazero => github.com/getlantern/wazero v1.11.0-water

require (
	github.com/gaukas/wazerofs v0.1.0
	github.com/tetratelabs/wazero v1.11.0
	google.golang.org/protobuf v1.33.0
)

require (
	github.com/blang/vfs v1.0.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
)

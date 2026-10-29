module github.com/a1ekseev/scd

go 1.27.0

toolchain go1.27.3

require (
	github.com/chromedp/chromedp v0.21.0
	github.com/klauspost/compress v1.20.3
	github.com/refraction-networking/utls v1.8.3-0.20260301010127-aa6edf4b11af // = the version xray-core requires; scripts/check-xray-version.sh
	github.com/robfig/cron/v3 v3.0.1
	github.com/spf13/cobra v1.10.2
	github.com/xtls/xray-core v1.260327.1-0.20261021115930-1b1647cd9ddf // = Xray v26.10.21 (1b1647c), stable; Go tag v1.261021.0 does not exist
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.61.0
	golang.org/x/term v0.47.0
	golang.org/x/text v0.43.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/andybalholm/brotli v1.2.7 // indirect
	github.com/apernet/quic-go v0.61.1-0.20260806010916-184d081eef3e // indirect
	github.com/chromedp/cdproto v0.158.6 // indirect
	github.com/go-json-experiment/json v0.0.0-20260820222146-c27c302e5fc3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/huin/goupnp v1.3.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackpal/go-nat-pmp v1.1.0 // indirect
	github.com/juju/ratelimit v1.0.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.1 // indirect
	github.com/koron/go-ssdp v0.9.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/libp2p/go-nat v1.0.1-0.20250821073202-01afc089f138 // indirect
	github.com/libp2p/go-netroute v0.4.0 // indirect
	github.com/miekg/dns v1.1.73 // indirect
	github.com/pion/dtls/v3 v3.1.15 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/stun/v3 v3.1.7 // indirect
	github.com/pion/transport/v4 v4.1.3 // indirect
	github.com/pion/transport/v5 v5.1.4 // indirect
	github.com/pires/go-proxyproto v0.16.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	github.com/xtls/reality v0.0.0-20261014101318-83f621788ca8 // indirect
	github.com/yuin/gopher-lua v1.1.2 // indirect
	go4.org/netipx v0.0.0-20260823151212-3075585bcbeb // indirect
	golang.org/x/crypto/x509roots/fallback v0.0.0-20261027005312-1da6254a58cf // indirect
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	golang.zx2c4.com/wireguard v0.0.0-20261024220952-b3bc704ef0ee // indirect
	golang.zx2c4.com/wireguard/windows v1.1.1 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261026165753-2ffab32bce7e // indirect
	layeh.com/gopher-luar v1.0.11 // indirect
	lukechampine.com/blake3 v1.4.1 // indirect
)

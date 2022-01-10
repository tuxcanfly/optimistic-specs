module github.com/ethereum-optimism/optimistic-specs

go 1.17

require (
	github.com/ethereum/go-ethereum v1.10.13
	github.com/holiman/uint256 v1.2.0
	github.com/protolambda/ask v0.1.2
	golang.org/x/term v0.0.0-20201126162022-7de9c90e9dd1
)

require (
	github.com/StackExchange/wmi v0.0.0-20180116203802-5d049714c4a6 // indirect
	github.com/btcsuite/btcd v0.20.1-beta // indirect
	github.com/deckarep/golang-set v0.0.0-20180603214616-504e848d77ea // indirect
	github.com/go-ole/go-ole v1.2.1 // indirect
	github.com/go-stack/stack v1.8.0 // indirect
	github.com/gorilla/websocket v1.4.2 // indirect
	github.com/shirou/gopsutil v3.21.4-0.20210419000835-c7a38de76ee5+incompatible // indirect
	github.com/tklauser/go-sysconf v0.3.5 // indirect
	github.com/tklauser/numcpus v0.2.2 // indirect
	golang.org/x/crypto v0.0.0-20210322153248-0c34fe9e7dc2 // indirect
	golang.org/x/sys v0.0.0-20210816183151-1e6c022a8912 // indirect
	gopkg.in/natefinch/npipe.v2 v2.0.0-20160621034901-c1b8fa8bdcce // indirect
)

replace github.com/ethereum/go-ethereum v1.10.13 => github.com/ethereum-optimism/reference-optimistic-geth v0.0.0-20220107224313-7f6d88bc156a

//replace github.com/ethereum/go-ethereum v1.10.13 => ../reference-optimistic-geth

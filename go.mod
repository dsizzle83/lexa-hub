module lexa-hub

go 1.26

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/godbus/dbus/v5 v5.2.2
	github.com/grandcat/zeroconf v1.0.0
	github.com/lorenzodonini/ocpp-go v0.19.0
)

require github.com/simonvetter/modbus v1.6.4 // indirect

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/go-playground/locales v0.12.1 // indirect
	github.com/go-playground/universal-translator v0.16.0 // indirect
	github.com/goburrow/serial v0.1.0 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/leodido/go-urn v1.1.0 // indirect
	github.com/miekg/dns v1.1.27 // indirect
	github.com/relvacode/iso8601 v1.6.0 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	gopkg.in/go-playground/validator.v9 v9.30.0 // indirect
	lexa-proto v0.0.0
)

replace lexa-proto => ../lexa-proto

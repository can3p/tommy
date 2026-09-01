module github.com/can3p/tommy/test/integration

go 1.26.0

require (
	github.com/can3p/tommy v0.0.0-00010101000000-000000000000
	github.com/mailjet/mailjet-apiv3-go/v4 v4.0.8
	github.com/sendgrid/rest v2.6.9+incompatible
	github.com/sendgrid/sendgrid-go v3.16.1+incompatible
	github.com/twilio/twilio-go v1.31.0
)

require (
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/emersion/go-smtp v0.25.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.2.2 // indirect
	github.com/golang/mock v1.6.0 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	golang.org/x/text v0.41.0 // indirect
)

replace github.com/can3p/tommy => ../..

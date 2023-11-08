# Vault SDK

[![Go Report Card](https://goreportcard.com/badge/github.com/bank-vaults/vault-sdk?style=flat-square)](https://goreportcard.com/report/github.com/bank-vaults/vault-sdk)
![Go Version](https://img.shields.io/badge/go%20version-%3E=1.19-61CFDD.svg?style=flat-square)
[![PkgGoDev](https://pkg.go.dev/badge/mod/github.com/bank-vaults/vault-sdk)](https://pkg.go.dev/mod/github.com/bank-vaults/vault-sdk)
<br>
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/bank-vaults/vault-sdk/ci.yaml?branch=main&style=flat-square)](https://github.com/bank-vaults/vault-sdk/actions/workflows/ci.yaml?query=workflow%3ACI)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/bank-vaults/vault-sdk/badge?style=flat-square)](https://api.securityscorecards.dev/projects/github.com/bank-vaults/vault-sdk)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/8048/badge)](https://www.bestpractices.dev/projects/8048)

**Go library for interacting with [Hashicorp Vault](https://www.vaultproject.io/).**

## Install

```shell
go get github.com/bank-vaults/vault-sdk
```

## Documentation

Check out the library documentation on the [Bank-Vaults website](https://bank-vaults.dev/docs/go-library/) or on [pkg.go.dev](https://pkg.go.dev/mod/github.com/bank-vaults/vault-sdk).

## Development

**For an optimal developer experience, it is recommended to install [Nix](https://nixos.org/download.html) and [direnv](https://direnv.net/docs/installation.html).**

_Alternatively, install [Go](https://go.dev/dl/) on your computer then run `make deps` to install the rest of the dependencies._

Fetch required tools:

```shell
make deps
```

Run the test suite:

```shell
make test
```

Run linters:

```shell
make lint # pass -j option to run them in parallel
```

Some linter violations can automatically be fixed:

```shell
make fmt
```

## License

The project is licensed under the [Apache 2.0 License](LICENSE).

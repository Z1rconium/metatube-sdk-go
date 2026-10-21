# MetaTube SDK Go

[![Build Status](https://img.shields.io/github/actions/workflow/status/metatube-community/metatube-sdk-go/docker.yml?branch=main&style=flat-square&logo=github-actions)](https://github.com/metatube-community/metatube-sdk-go/actions/workflows/release.yml)
[![Go Linter](https://img.shields.io/github/actions/workflow/status/metatube-community/metatube-sdk-go/linter.yml?style=flat-square&branch=main&logo=githubactions&label=golangci-lint)](https://github.com/metatube-community/metatube-sdk-go/actions/workflows/linter.yml)
[![Require Go Version](https://img.shields.io/badge/go-%3E%3D1.25-30dff3?style=flat-square&logo=go)](https://github.com/metatube-community/metatube-sdk-go/blob/main/go.mod)
[![GitHub License](https://img.shields.io/github/license/metatube-community/metatube-sdk-go?color=e4682a&logo=apache&style=flat-square)](https://github.com/metatube-community/metatube-sdk-go/blob/main/LICENSE)
[![Tag](https://img.shields.io/github/v/tag/metatube-community/metatube-sdk-go?color=%23ff8936&logo=fitbit&style=flat-square)](https://github.com/metatube-community/metatube-sdk-go/tags)

Metadata Tube SDK in Golang.

## Contents

- [MetaTube SDK Go](#metatube-sdk-go)
    - [Contents](#contents)
    - [Features](#features)
    - [Installation](#installation)
    - [Region-restricted providers](#region-restricted-providers)
    - [Credits](#credits)
    - [License](#license)

## Features

- Supported platforms
    - Linux
    - Darwin
    - Windows
    - BSD(s)
- Supported Databases
    - [SQLite](https://gitlab.com/cznic/sqlite)
    - [PostgreSQL](https://github.com/jackc/pgx)
- Image processing
    - Auto cropping
    - Badge support
    - Face detection
    - Image hashing
- RESTful API
- 20+ providers
- Text translation

## Installation

To install this package, you first need [Go](https://golang.org/) installed (**go1.25+ is required**), then you can use
the below Go command to install SDK.

```sh
go get -u github.com/metatube-community/metatube-sdk-go
```

## Region-restricted providers

Some providers are only accessible from a specific region, e.g. the mono/DVD pages of
`FANZA` (DMM) are restricted to Japan. Such providers can be accessed through a foreign
exit IP by configuring a proxy for them.

The recommended setup is an [aimili-vpngate](https://github.com/OpenMili/aimili-vpngate)
gateway, which runs a VPNGate tunnel on a Linux host and exposes it as a local
HTTP/HTTPS/SOCKS5 proxy. Its management API is used to acquire a fresh exit IP on demand.

### Configuration

| Config key       | Description                                                                                 |
|------------------|---------------------------------------------------------------------------------------------|
| `proxy`          | The http or socks5 proxy endpoint, `http://127.0.0.1:7928` by default                       |
| `vpngate_url`    | The base URL of the gateway management API, including the secret path                        |
| `vpngate_username` | The username of the gateway management backend                                             |
| `vpngate_password` | The password of the gateway management backend                                             |
| `vpngate_country`  | The ISO country code of the exit IP, `JP` by default                                      |

```sh
export MT_MOVIE_PROVIDER_FANZA__VPNGATE_URL=http://admin:secret@127.0.0.1:8787/YOUR_SECRET_PATH
export MT_MOVIE_PROVIDER_FANZA__VPNGATE_USERNAME=admin
export MT_MOVIE_PROVIDER_FANZA__VPNGATE_PASSWORD=secret
export MT_MOVIE_PROVIDER_FANZA__PROXY=http://127.0.0.1:7928
```

The credentials may also be embedded in the URL, e.g.
`MT_MOVIE_PROVIDER_FANZA__VPNGATE_URL=http://admin:secret@127.0.0.1:8787/YOUR_SECRET_PATH`.

### Fallback behavior

When the exit IP turns out to be unusable, e.g. the VPN tunnel is down or the page is
still region-restricted, the provider falls back as follows:

1. The request is sent through the proxy.
2. On failure, a new exit IP is acquired from the gateway and the request is retried once.
   - If the gateway is already switching nodes by itself (e.g. after the tunnel died), the
     provider waits for it to settle instead of sending a competing switch request.
   - Concurrent failures of the same exit IP are coalesced into a single switch.
   - If the best node cannot be connected, the next best nodes are tried.
3. If it still fails, the provider falls back to the default server egress for one minute,
   and then resumes the gateway proxy on the next request. If it is still unusable, it falls
   back again. The default server egress is the proxy that you have configured for the
   server, e.g. `HTTP_PROXY` / `HTTPS_PROXY`, or direct egress if there is none.

### Deployment notes

- The gateway requires a Linux host with root privileges and a TUN device, i.e. it cannot
  run on macOS or Windows.
- Forward both the proxy port and the management port to the client, e.g.
  `ssh -N -L 7928:127.0.0.1:7928 -L 8787:127.0.0.1:8787 root@VPS`.
- The gateway must be configured with the target country, e.g. the routing mode
  `fixed_region` with `force_country` set to `JP`, otherwise the exit IP may not be
  located in the requested region.
- The gateway only takes over the providers that are configured with `vpngate_url`, i.e. the
  region-restricted ones such as `FANZA`. All the other providers keep using whatever you
  have configured for them, i.e. their own `proxy` config, or `HTTP_PROXY` / `HTTPS_PROXY`,
  or direct egress.
- A plain `proxy` config without `vpngate_url` is used as is: the provider never bypasses
  it nor falls back to direct egress.

## Credits

| Library														                                           | Description																						                                                                    |
|-----------------------------------------------------------------|------------------------------------------------------------------------------------------------------|
| [gocolly/colly](https://github.com/gocolly/colly)			            | Elegant Scraper and Crawler Framework for Golang													                                        |
| [gin-gonic/gin](https://github.com/gin-gonic/gin)			            | Gin is a HTTP web framework written in Go															                                             |
| [gorm.io/gorm](https://gorm.io/)								                        | The fantastic ORM library for Golang																                                                 |
| [esimov/pigo](https://github.com/esimov/pigo)				               | Fast face detection, pupil/eyes localization and facial landmark points detection library in pure Go |
| [robertkrimen/otto](https://github.com/robertkrimen/otto)       | A JavaScript interpreter in Go (golang)                                                              |
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)		         | Package sqlite is a CGo-free port of SQLite/SQLite3												                                      |
| [corona10/goimagehash](https://github.com/corona10/goimagehash) | Go Perceptual image hashing package																                                                  |
| [antchfx/xpath](https://github.com/antchfx/xpath)			            | XPath package for Golang, supports HTML, XML, JSON document query									                           |
| [gen2brain/jpegli](https://github.com/gen2brain/jpegli)         | Go encoder/decoder for JPEG based on jpegli                                                          |

## License

[Apache-2.0 License](https://github.com/metatube-community/metatube-sdk-go/blob/main/LICENSE)

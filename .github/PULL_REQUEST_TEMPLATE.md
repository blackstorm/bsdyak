## Summary
Describe the change and why it is needed.

## Checklist
- [ ] Pure Go (`CGO_ENABLED=0` builds cleanly for `freebsd/amd64` and `freebsd/arm64`)
- [ ] No new public ports or modifications to host `/etc/pf.conf` / `/etc/rc.conf` / `/boot/loader.conf`
- [ ] `go vet ./...` and `go test -race ./...` pass
- [ ] Updated docs (`website/` and `website/zh/`) if CLI or `Yakfile` behavior changed

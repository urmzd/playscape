# Changelog

## 0.11.0 (2026-10-31)

### Breaking

- **isolate**: give each instance its own /project and /me workspaces (#20) ([41558c5](https://github.com/urmzd/playscape/commit/41558c54d708ba129908addcc9b3249089ace613))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.10.0...v0.11.0)


## 0.10.0 (2026-10-30)

### Breaking

- **isolate**: require gVisor and a stricter seccomp profile in the docker driver (#19) ([a55543f](https://github.com/urmzd/playscape/commit/a55543f8389adfc55db104a0948ca00a2c83045a))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.9.0...v0.10.0)


## 0.9.0 (2026-10-30)

### Features

- **isolate**: add the egress gate with credential injection (#18) ([b901f25](https://github.com/urmzd/playscape/commit/b901f2514212952c4228bc4176de6ef7aedd973e))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.8.0...v0.9.0)


## 0.8.0 (2026-10-30)

### Features

- **isolate**: run a run's tools in pooled, isolated instances (#17) ([d8415f6](https://github.com/urmzd/playscape/commit/d8415f6ec77c02601d8781311eaa500365ddfb79))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.7.2...v0.8.0)


## 0.7.2 (2026-10-29)

### .

### pkg/runner/k8s

#### Bug Fixes

- **deps**: patch vulnerable Go dependencies (#16) ([6f303fa](https://github.com/urmzd/playscape/commit/6f303fab274e6c6b869d3fb786c13e13c2098dbc))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.7.1...v0.7.2)


## 0.7.1 (2026-10-21)

### Bug Fixes

- **k8s**: require playscape v0.7.0, the first release without the package (#13) ([7c9297a](https://github.com/urmzd/playscape/commit/7c9297af1bf20521bd9b5c21fb4afe347b69209e))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.7.0...v0.7.1)


## 0.7.0 (2026-10-21)

### Breaking

- playscape is a library with a playscape run CLI; the server and its queue are gone (#12) ([7ff97a4](https://github.com/urmzd/playscape/commit/7ff97a4a36f9fee2b5ad4f2b72865bfea543f8fb))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.6.1...v0.7.0)


## 0.6.1 (2026-10-20)

### Bug Fixes

- exact CLI arguments, Kubernetes expansion and timeouts, process-group cancel (#11) ([ee48cda](https://github.com/urmzd/playscape/commit/ee48cda705e0fdea52471757d3e405a3f207ddcf))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.6.0...v0.6.1)


## 0.5.0 (2026-10-20)

### Features

- **runner**: add Job-backed sandboxed shell executor (#9) ([6ab98ed](https://github.com/urmzd/playscape/commit/6ab98ed27691e54e6345bfe5829c0b01bf62f126))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.4.0...v0.5.0)


## 0.4.0 (2026-10-18)

### Features

- **tool**: add cli provider kind wrapping CLI binaries (#8) ([18d2b1c](https://github.com/urmzd/playscape/commit/18d2b1cd9f20981c5f7f52da06459d4e5105239d))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.3.0...v0.4.0)


## 0.3.0 (2026-10-18)

### Breaking

- rename legatus to playscape (#6) ([f5e0921](https://github.com/urmzd/playscape/commit/f5e0921266bbf3af68e43c47205acc6fda05d81d))

### Misc

- **deps**: upgrade saige example to v0.37.0 (#5) ([efa3091](https://github.com/urmzd/playscape/commit/efa309174e91a4751578248c025de1657f1ddb34))

[Full Changelog](https://github.com/urmzd/playscape/compare/v0.2.0...v0.3.0)


## 0.2.0 (2026-10-10)

### Breaking

- rename project from dispatch to legatus (#4) ([d4a21bd](https://github.com/urmzd/legatus/commit/d4a21bd34344b94005e3e38c15fb956c7cf217b3))

### Misc

- **release**: read the sr-releaser key from the release environment (#3) ([3053203](https://github.com/urmzd/legatus/commit/305320341aa3207ac23f132fda454e93d024fa54))
- upgrade urmzd/sr action to v9 (#2) ([37bef3c](https://github.com/urmzd/legatus/commit/37bef3c19dde64f0a4e22f998edf4a3297ddd60b))

[Full Changelog](https://github.com/urmzd/legatus/compare/v0.1.0...v0.2.0)


## 0.1.0 (2026-07-10)

### Features

- define access with an NGAC policy machine ([525ece8](https://github.com/urmzd/dispatch/commit/525ece8da568be80bcdf26b7edd29aea7ef8b502))

### Misc

- initial project scaffold ([f42bb70](https://github.com/urmzd/dispatch/commit/f42bb709408156dfd3016bf6e77ff3e89cc96974))

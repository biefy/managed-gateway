# Third-party forks

This repository vendors the exact fork contents used by the managed gateway MVP. The fork repositories are private and are the source of record for upstream-specific changes.

## Fork references

| Component | Private fork | Base upstream commit | MVP commit |
| --- | --- | --- | --- |
| Istio | `https://github.com/biefy/istio` | `44d0e58e49d0dc89e27fc4f8679c68132d46b887` | `267017f76122d4051289e7aa7e0b68c345dd5a71` |
| agentgateway | `https://github.com/biefy/agentgateway` | `2cede80015e068617c014160d3e3273907bd5f99` | `2ad516159de189b20f753650468171b177de2a55` |

## Local layout

`third_party/istio` and `third_party/agentgateway` are vendored source directories, not git submodules. Their nested `.git` directories are intentionally removed in this repo so clone/build behavior does not depend on submodule initialization.

To refresh either fork manually:

```sh
git clone https://github.com/biefy/istio /tmp/istio
cd /tmp/istio
git checkout 267017f76122d4051289e7aa7e0b68c345dd5a71
```

```sh
git clone https://github.com/biefy/agentgateway /tmp/agentgateway
cd /tmp/agentgateway
git checkout 2ad516159de189b20f753650468171b177de2a55
```

Each fork contains a `ChangeSummary.md` at its root with the upstream-specific change summary.

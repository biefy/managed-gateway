# Change Summary

## Merge Istio upstream master

- Fetched `github.com/istio/istio` `master` into `upstream/master`.
- Merged upstream Istio history into this repository with `--allow-unrelated-histories`.
- Kept the managed-gateway root module path (`gateway.appnet.azure.com/controller`) and managed-gateway README during conflict resolution.
- Combined local and upstream `.gitignore` entries.
- Validated the merge with `go test ./controller/...` and `git diff --check`.

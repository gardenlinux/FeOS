# ADR 0025: Private images: a create brings its registry credentials, the registry is asked before every new pod, and an image is baked once behind that question

**Status:** Merged into [ADR 0022](0022-images.md) · **Date:** 2026-10-29

It decided that a create brings its own registry credentials, never the
tenant's; that `ferund` asks the image's registry with them before every
new pod, cached image or not, and remembers nothing but the registry's
token; and that an image is baked once, in its first pod, keyed by its
source and used only after the yes. All of it lives in ADR 0022, with
the guest's pull on the registry's token as the one credential in a pod.
The node's own pull, which ends that, is ADR 0050.

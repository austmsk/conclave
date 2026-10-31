// Package deployfile holds what every loader of the owner's deploy/*.yaml
// files does the same way: decode exactly one YAML document with unknown
// keys refused (decision 0007), check repository keys (the owner/repo
// placeholder, the full-name shape, and two keys that differ only in
// case), and open a file and record the SHA-256 of the bytes that were
// validated, so a plan can name the configuration it ran under.
//
// Each loader keeps its own sentinel error and its own validation of what
// a repository entry holds; only the shared mechanics live here.
package deployfile

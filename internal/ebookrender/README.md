# Bounded EPUB text foundation

`Parse(ctx, readerAt, size)` admits DRM-free, reflowable EPUB archives and returns
spine-ordered normalized XHTML body text. It does not write files or fetch any
resource. The package is not wired to an API or an advertised capability.

Admission limits are 32 MiB compressed, 1,024 entries, 4 MiB per expanded entry,
32 MiB total expanded, and XML nesting depth 128. Archive names and manifest
references must be canonical relative paths; encoded, absolute, parent traversal,
query, fragment, external and backslash paths are rejected conservatively.
Encryption declarations, fixed-layout metadata, malformed XML, DTDs, unknown
spine references, duplicate entry/manifest IDs and repeated spine IDs are rejected.
All declared manifest resources must exist. Scripts/styles are ignored; image and
link targets are never loaded. XML entity resolution is disabled by Go's decoder.
Jobs remain synchronous: callers must bound concurrent work and provide a deadline.
Cancellation is checked between bounded entry reads and XML tokens; it does not
interrupt a blocked caller-provided ReaderAt.

`Book.Pages(runesPerPage)` emits at most 10,000 plain text chunks, each containing
at most 16,384 Unicode scalar values. These are chunks for a future native text
reader, not measured visual pages. Illustrations, tables, CSS typography, complex
layout, grapheme-aware line breaking and TOC extraction are not provided. Strict
XML handling excludes EPUBs using HTML named entities or a DOCTYPE.

Locations use `silo-text-v1:<archive-sha256>:<spine-index>:<rune-offset>` over
normalized text. They are stable for the same archive and extraction version and
independent of chunk size. They are not interoperable EPUB CFI, do not survive an
archive revision, and must never replace existing browser-reader CFI progress.
An API integration still needs authorization, revision/layout negotiation,
location resolution, bounded scheduling and explicit capability negotiation.

#!/usr/bin/env bash
# Render a Structurizr workspace's Documentation tab and views as one PDF.
#
# Run from the repository root. It exports every view as PNG plus the workspace
# as JSON into <arch-dir>/generated/ (keep that folder gitignored), assembles
# the Markdown with build_architecture_pdf_source.py, and renders it with
# Pandoc and the Eisvogel template. The PDF lands beside the exports, named
# <project>-architecture-<date>-<edition>.pdf.
#
# Mermaid diagrams (fences in the text, or the links Structurizr's Mermaid
# plugin leaves in ADRs) become images too: the builder saves each one's source
# to <arch-dir>/generated/mermaid-pdf/, render-mermaid.sh renders them with
# MERMAID_IMAGE before Pandoc runs, and a workspace without Mermaid skips the
# step and never pulls the image.
#
# An operator tool: run it by hand, or let the Docs / Architecture PDF workflow
# run it through `make pdf` when an operator starts that workflow on main.
# Never trigger it automatically.
#
#   STRUCTURIZR_IMAGE  required: the pinned Structurizr image your viewer runs,
#                      e.g. structurizr/structurizr:2026.10.26 (the PNG export
#                      uses its -playwright tag)
#   PANDOC_IMAGE       optional: default pandoc/extra:3.14.0.0-debian, the
#                      version this was tested with (LaTeX, Eisvogel; ~2 GB)
#   ARCH_DIR           optional: default docs/architecture
#   MERMAID_IMAGE      optional: default minlag/mermaid-cli:12.0.1, the
#                      Mermaid CLI that renders the diagrams (~630 MB). Keep
#                      it identical to the Makefile's pin.
set -euo pipefail

: "${STRUCTURIZR_IMAGE:?Set STRUCTURIZR_IMAGE to your pinned Structurizr image}"
PANDOC_IMAGE="${PANDOC_IMAGE:-pandoc/extra:3.14.0.0-debian}"
ARCH_DIR="${ARCH_DIR:-docs/architecture}"
MERMAID_IMAGE="${MERMAID_IMAGE:-minlag/mermaid-cli:12.0.1}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
arch="$(cd "${ARCH_DIR}" && pwd)"
generated="${arch}/generated"

mkdir -p "${generated}"
# The Structurizr container writes as its own user.
chmod 777 "${generated}"

docker run --rm -v "${arch}:/w:ro" -v "${generated}:/out" "${STRUCTURIZR_IMAGE}" \
  export -workspace /w/workspace.dsl -format json -output /out
docker run --rm -v "${arch}:/w:ro" -v "${generated}:/out" "${STRUCTURIZR_IMAGE}-playwright" \
  export -workspace /w/workspace.dsl -format png -output /out

pdf="$(python3 "${SCRIPT_DIR}/build_architecture_pdf_source.py" \
  "${arch}" "${generated}" "${generated}/architecture.md")"

if compgen -G "${generated}/mermaid-pdf/*.mmd" >/dev/null; then
  MERMAID_IMAGE="${MERMAID_IMAGE}" \
    "${SCRIPT_DIR}/render-mermaid.sh" "${generated}/mermaid-pdf"
fi

docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -v "${arch}:/data" -w /data \
  "${PANDOC_IMAGE}" generated/architecture.md -o "generated/${pdf}" \
  --template eisvogel --pdf-engine=xelatex --resource-path=/data

echo "wrote ${ARCH_DIR}/generated/${pdf}"

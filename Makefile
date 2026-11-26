.PHONY: docs-venv docs-check

ZENSICAL ?= .venv/bin/zensical
ifeq ($(ZENSICAL),.venv/bin/zensical)
DOCS_VENV_DEP = .venv/docs-requirements.stamp
endif

# docs-venv creates .venv and installs the hash-locked docs site requirements.
docs-venv: .venv/docs-requirements.stamp

# The stamp records a finished install of the locked requirements, so a zensical placed in .venv some
# other way, or an install that stopped half way, does not count.
.venv/docs-requirements.stamp: requirements-docs.txt
	python3 -m venv .venv
	.venv/bin/pip install --require-hashes -r requirements-docs.txt
	touch $@

# docs-check fails when the Zensical build reports an issue, or when the built site has a broken
# internal link, broken anchor, missing image or third-party asset.
docs-check: $(DOCS_VENV_DEP)
	$(ZENSICAL) build --clean --strict
	python3 -I hack/docs-site-check.py check site

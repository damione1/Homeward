FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY . .
RUN test "$(go env GOVERSION)" = go1.27.2 \
 && go build -mod=readonly -trimpath -buildvcs=false -tags timetzdata -o /out/homed ./cmd/homed \
 && go build -mod=readonly -trimpath -buildvcs=false -tags timetzdata -o /out/homeward ./cmd/homeward \
 && go build -mod=readonly -trimpath -buildvcs=false -tags timetzdata -o /out/homeward-package ./cmd/homeward-package
RUN install -d -m 0700 -o 10001 -g 10001 /seed/var/lib/homeward /seed/run/homeward-credentials /seed/existing-postgres

FROM postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f AS postgres-client
# Keep this image's ELF interpreter and complete ldd closure isolated from the
# Python worker's system libraries. The final runtime must execute both tools.
RUN set -eu; \
    install -d /export/opt/homeward/pg-client/bin /export/opt/homeward/pg-client/lib; \
    for tool in pg_dump pg_restore; do \
      cp /usr/lib/postgresql/17/bin/$tool /export/opt/homeward/pg-client/bin/$tool.real; \
      ldd /usr/lib/postgresql/17/bin/$tool >> /tmp/client-dependencies; \
    done; \
    ! grep -q 'not found' /tmp/client-dependencies; \
    cp /tmp/client-dependencies /export/opt/homeward/pg-client/source-linkage.txt; \
    printf '%s\n' 'postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f' > /export/opt/homeward/pg-client/source-image.txt; \
    awk '/=> \// {print $3} /^[[:space:]]*\// {print $1}' /tmp/client-dependencies | sort -u | \
      while read library; do cp -L "$library" /export/opt/homeward/pg-client/lib/; done; \
    loader=$(awk '/ld-linux/ {for(i=1;i<=NF;i++) if($i ~ /^\//) {print $i;exit}}' /tmp/client-dependencies); \
    test -n "$loader"; \
    for tool in pg_dump pg_restore; do \
      printf '#!/bin/sh\nexec /opt/homeward/pg-client/lib/%s --library-path /opt/homeward/pg-client/lib /opt/homeward/pg-client/bin/%s.real "$@"\n' "${loader##*/}" "$tool" > /export/opt/homeward/pg-client/bin/$tool; \
      chmod 0555 /export/opt/homeward/pg-client/bin/$tool; \
    done

FROM python:3.13.17-slim-bookworm@sha256:ac411330d543edd1cc55be253a783cff743082feddfa07957a4574ad1ea01a6b AS runtime
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY --from=postgres-client /export/ /
COPY packaging/requirements.lock /opt/homeward/requirements.lock
RUN python -m pip install --no-cache-dir --only-binary=:all: --require-hashes --no-deps -r /opt/homeward/requirements.lock \
 && python -m pip check
COPY drivers/caseta /opt/homeward/drivers/caseta
COPY testworker/generated /opt/homeward/testworker/generated
COPY testworker/schema_digest.py /opt/homeward/testworker/schema_digest.py
COPY --from=build /out/ /usr/local/bin/
COPY --from=build /seed/ /
COPY packaging/versions.json /opt/homeward/versions.json
WORKDIR /opt/homeward
USER 10001:10001
RUN /opt/homeward/pg-client/bin/pg_dump --version \
 && /opt/homeward/pg-client/bin/pg_restore --version
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/homeward-package", "serve"]

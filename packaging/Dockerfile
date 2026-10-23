FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ARG SOURCE_DATE_EPOCH
ARG TARGETOS
ARG TARGETARCH
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}
WORKDIR /src
COPY . .
RUN test "$(go env GOVERSION)" = go1.27.2 \
 && go build -mod=readonly -trimpath -buildvcs=false -ldflags=-buildid= -tags timetzdata -o /out/homed ./cmd/homed \
 && go build -mod=readonly -trimpath -buildvcs=false -ldflags=-buildid= -tags timetzdata -o /out/homeward ./cmd/homeward \
 && go build -mod=readonly -trimpath -buildvcs=false -ldflags=-buildid= -tags timetzdata -o /out/homeward-package ./cmd/homeward-package \
 && if test -d cmd/homeward-observe; then go build -mod=readonly -trimpath -buildvcs=false -ldflags=-buildid= -tags timetzdata -o /out/homeward-observe ./cmd/homeward-observe; fi
RUN mkdir -p /release/licenses/go /release/licenses/product /release/licenses/go-toolchain \
 && go list -m -json all > /release/go-modules.json \
 && go list -m -f '{{if not .Main}}{{if .Replace}}{{printf "%s\t%s\t%s" .Path .Version .Replace.Dir}}{{else}}{{printf "%s\t%s\t%s" .Path .Version .Dir}}{{end}}{{end}}' all > /tmp/go-modules.tsv \
 && while IFS="$(printf '\t')" read -r module version directory; do \
      test -d "$directory" || continue; \
      destination="/release/licenses/go/$module@$version"; \
      mkdir -p "$destination"; \
      find "$directory" -type f \( -iname '*license*' -o -iname '*licence*' -o -iname '*notice*' -o -iname '*copyright*' -o -iname '*copying*' -o -iname 'authors*' \) -exec sh -c 'source=$1; destination=$2; shift 2; for f; do relative=${f#"$source"/}; mkdir -p "$destination/$(dirname "$relative")"; cp "$f" "$destination/$relative"; done' sh "$directory" "$destination" {} +; \
    done < /tmp/go-modules.tsv \
 && cp LICENSE internal/calendars/THIRD-PARTY-NOTICES.txt internal/solar/LICENSE.astral /release/licenses/product/ \
 && cp third_party/homekit/NOTICE /release/licenses/product/homekit-NOTICE \
 && cp qualification/ecobee/third_party/NOTICE /release/licenses/product/hc-NOTICE \
 && cp third_party/homekit/UPSTREAM.json qualification/ecobee/third_party/hc.upstream.json /release/ \
 && cp /usr/local/go/LICENSE /usr/local/go/PATENTS /release/licenses/go-toolchain/ \
 && cp /usr/local/go/lib/time/README /release/licenses/go-toolchain/tzdata-README \
 && find /usr/local/go -type f \( -iname '*license*' -o -iname '*notice*' -o -iname '*copyright*' -o -iname 'authors*' \) -exec sh -c 'for f; do relative=${f#/usr/local/go/}; destination=/release/licenses/go-toolchain/full; mkdir -p "$destination/$(dirname "$relative")"; cp "$f" "$destination/$relative"; done' sh {} +
RUN install -d -m 0700 -o 10001 -g 10001 /seed/var/lib/homeward /seed/run/homeward-credentials /seed/existing-postgres

FROM postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f AS postgres-client
# Keep this image's ELF interpreter and complete ldd closure isolated from the
# Python worker's system libraries. The final runtime must execute both tools.
RUN set -eu; \
    install -d /export/opt/homeward/pg-client/bin /export/opt/homeward/pg-client/lib; \
    for tool in pg_dump pg_restore; do \
      cp /usr/lib/postgresql/17/bin/$tool /export/opt/homeward/pg-client/bin/$tool.real; \
      printf '/usr/lib/postgresql/17/bin/%s\t/opt/homeward/pg-client/bin/%s.real\n' "$tool" "$tool" >> /tmp/client-copies; \
      ldd /usr/lib/postgresql/17/bin/$tool >> /tmp/client-dependencies; \
    done; \
    ! grep -q 'not found' /tmp/client-dependencies; \
    cp /tmp/client-dependencies /export/opt/homeward/pg-client/source-linkage.txt; \
    printf '%s\n' 'postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f' > /export/opt/homeward/pg-client/source-image.txt; \
    awk '/=> \// {print $3} /^[[:space:]]*\// {print $1}' /tmp/client-dependencies | sort -u | \
      while read library; do cp -L "$library" /export/opt/homeward/pg-client/lib/; printf '%s\t/opt/homeward/pg-client/lib/%s\n' "$library" "${library##*/}" >> /tmp/client-copies; done; \
    loader=$(awk '/ld-linux/ {for(i=1;i<=NF;i++) if($i ~ /^\//) {print $i;exit}}' /tmp/client-dependencies); \
    test -n "$loader"; \
    for tool in pg_dump pg_restore; do \
      printf '#!/bin/sh\nexec /opt/homeward/pg-client/lib/%s --library-path /opt/homeward/pg-client/lib /opt/homeward/pg-client/bin/%s.real "$@"\n' "${loader##*/}" "$tool" > /export/opt/homeward/pg-client/bin/$tool; \
      chmod 0555 /export/opt/homeward/pg-client/bin/$tool; \
    done; \
    mkdir -p /export/opt/homeward/release/licenses/postgres; \
    dpkg-query -W -f='${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\t${Architecture}\n' > /export/opt/homeward/release/donor-packages.tsv; \
    while IFS="$(printf '\t')" read -r original destination; do \
      package=$(dpkg-query -S "$original" 2>/dev/null | head -1 | sed 's/: \/.*//'); \
      if test -z "$package"; then package=$(dpkg-query -S "$(readlink -f "$original")" | head -1 | sed 's/: \/.*//'); fi; \
      test -n "$package"; \
      digest=$(sha256sum "$original" | cut -d' ' -f1); \
      printf '%s\t%s\t%s\t%s\n' "$destination" "$original" "$digest" "$package" >> /export/opt/homeward/release/donor-copies.tsv; \
      directory="/usr/share/doc/${package%%:*}"; \
      test -d "$directory"; \
      mkdir -p "/export/opt/homeward/release/licenses/postgres/$package"; \
      cp -aL "$directory/." "/export/opt/homeward/release/licenses/postgres/$package/"; \
    done < /tmp/client-copies

FROM python:3.13.17-slim-bookworm@sha256:ac411330d543edd1cc55be253a783cff743082feddfa07957a4574ad1ea01a6b AS runtime
ARG SOURCE_DATE_EPOCH
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}
COPY --from=postgres-client /export/ /
COPY packaging/requirements.lock /opt/homeward/requirements.lock
RUN mkdir -p /opt/homeward/release \
 && python -m pip install --report /opt/homeward/release/python-install.json --no-compile --no-cache-dir --only-binary=:all: --require-hashes --no-deps -r /opt/homeward/requirements.lock \
 && python -m pip check
COPY --from=build /release/ /opt/homeward/release/
COPY packaging/collect-runtime.py /tmp/homeward-collect-runtime.py
RUN python /tmp/homeward-collect-runtime.py runtime --output /opt/homeward/release \
 && rm /tmp/homeward-collect-runtime.py
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

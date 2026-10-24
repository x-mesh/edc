FROM scratch
ARG TARGETARCH
COPY bin/edc-collector-linux-${TARGETARCH} /edc-collector
LABEL org.x-mesh.edc.collector.protocol="1"
USER 0:0
ENTRYPOINT ["/edc-collector"]

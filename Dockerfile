# The Deskbridge relay as a container (see cmd/deskbridge-relay and the
# INTERNET section of README.txt). The release workflow builds the program
# first; this only wraps it. Nothing else is in the image.
FROM scratch
COPY dist/deskbridge-relay /deskbridge-relay
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/deskbridge-relay"]

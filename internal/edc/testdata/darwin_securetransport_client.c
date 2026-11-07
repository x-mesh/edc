#include <Security/SecureTransport.h>
#include <arpa/inet.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

static OSStatus read_tls(SSLConnectionRef connection, void *data, size_t *length) {
    int fd = *(const int *)connection;
    size_t requested = *length;
    *length = 0;
    while (*length < requested) {
        ssize_t n = recv(fd, (char *)data + *length, requested - *length, 0);
        if (n > 0) *length += (size_t)n;
        else if (n == 0) return errSSLClosedGraceful;
        else if (errno != EINTR) return errSSLClosedAbort;
    }
    return noErr;
}

static OSStatus write_tls(SSLConnectionRef connection, const void *data, size_t *length) {
    int fd = *(const int *)connection;
    size_t requested = *length;
    *length = 0;
    while (*length < requested) {
        ssize_t n = send(fd, (const char *)data + *length, requested - *length, 0);
        if (n > 0) *length += (size_t)n;
        else if (n == 0 || errno != EINTR) return errSSLClosedAbort;
    }
    return noErr;
}

int main(int argc, char **argv) {
    if (argc != 2) return 2;
    int port = atoi(argv[1]);
    if (port < 1 || port > 65535) return 2;
    SSLContextRef context = NULL;
    if (SSLNewContext(false, &context) != noErr) return 3;
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) { SSLDisposeContext(context); return 4; }
    struct timeval timeout = {5, 0};
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    int result = 5;
    if (SSLSetIOFuncs(context, read_tls, write_tls) != noErr ||
        SSLSetConnection(context, &fd) != noErr ||
        SSLSetProtocolVersionMin(context, kTLSProtocol12) != noErr) goto done;
    /* This fixture connects only to the local server with a generated certificate. */
    if (SSLSetEnableCertVerify(context, false) != noErr) goto done;
    puts("EDC_CLIENT_READY");
    fflush(stdout);
    if (getchar() == EOF) goto done;
    struct sockaddr_in address = {0};
    address.sin_family = AF_INET;
    address.sin_port = htons((unsigned short)port);
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (connect(fd, (struct sockaddr *)&address, sizeof(address)) != 0) goto done;
    OSStatus status;
    do { status = SSLHandshake(context); } while (status == errSSLWouldBlock);
    if (status != noErr) goto done;
    const char request[] = "GET /keyless-probe HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer fixture-secret\r\nConnection: close\r\n\r\n";
    size_t offset = 0;
    while (offset < sizeof(request) - 1) {
        size_t processed = 0;
        status = SSLWrite(context, request + offset, sizeof(request) - 1 - offset, &processed);
        if ((status != noErr && status != errSSLWouldBlock) || processed == 0) goto done;
        offset += processed;
    }
    char response[4096] = {0};
    size_t received = 0;
    while (received < sizeof(response) - 1) {
        size_t processed = 0;
        status = SSLRead(context, response + received, sizeof(response) - 1 - received, &processed);
        if (processed > sizeof(response) - 1 - received) goto done;
        received += processed;
        char *body = strstr(response, "\r\n\r\n");
        if (body && received >= (size_t)(body + 4 - response) + 4) {
            const unsigned char expected[] = {'o', 0, 'k', 255};
            if (strstr(response, "HTTP/1.1 200 OK") && memcmp(body + 4, expected, 4) == 0) result = 0;
            break;
        }
        if ((status != noErr && status != errSSLWouldBlock) || processed == 0) break;
    }
    if (result == 0) SSLClose(context);
done:
    SSLDisposeContext(context);
    close(fd);
    return result;
}

#include <arpa/inet.h>
#include <openssl/ssl.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>
#include <stdlib.h>

int main(int argc, char **argv) {
    if (argc < 2 || argc > 4) return 2;
    int extended = argc > 2 && strcmp(argv[2], "ex") == 0;
    int hold = argc > 3 && strcmp(argv[3], "hold") == 0;
    int port = atoi(argv[1]);
    if (port < 1 || port > 65535) return 2;
    SSL_CTX *context = SSL_CTX_new(TLS_client_method());
    if (!context) return 3;
    /* The peer is a local httptest server with a generated certificate. */
    SSL_CTX_set_verify(context, SSL_VERIFY_NONE, NULL);
    SSL *ssl = SSL_new(context);
    if (!ssl) return 4;
    puts("EDC_CLIENT_READY");
    fflush(stdout);
    if (getchar() == EOF) return 5;
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) return 6;
    struct timeval timeout = {5, 0};
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
    struct sockaddr_in address = {0};
    address.sin_family = AF_INET;
    address.sin_port = htons((unsigned short)port);
    address.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    int result = 7;
    if (connect(fd, (struct sockaddr *)&address, sizeof(address)) != 0) goto done;
    if (SSL_set_fd(ssl, fd) != 1 || SSL_connect(ssl) != 1) goto done;
    const char request[] = "GET /keyless-probe HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer fixture-secret\r\nConnection: close\r\n\r\n";
    if (extended) {
        size_t written = 0;
        if (SSL_write_ex(ssl, request, strlen(request), &written) != 1 || written != strlen(request)) goto done;
    } else if (SSL_write(ssl, request, (int)strlen(request)) != (int)strlen(request)) goto done;
    char response[4096] = {0};
    int count;
    if (extended) {
        size_t read = 0;
        if (SSL_read_ex(ssl, response, sizeof(response) - 1, &read) != 1) goto done;
        count = (int)read;
    } else {
        count = SSL_read(ssl, response, sizeof(response) - 1);
    }
    if (count > 0 && strstr(response, "HTTP/1.1 200 OK")) result = 0;
    if (hold && result == 0) getchar();
done:
    SSL_free(ssl);
    SSL_CTX_free(context);
    close(fd);
    return result;
}

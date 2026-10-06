#include <openssl/ssl.h>
#include <stddef.h>
#include <string.h>

int send_http1(SSL *ssl) {
    static const char request[] =
        "GET /codex/install.sh?token=EDC_SECRET_QUERY HTTP/1.1\r\n"
        "Host: chatgpt.com\r\n"
        "Authorization: Bearer EDC_SECRET_HEADER\r\n"
        "Content-Length: 15\r\n\r\n"
        "EDC_SECRET_BODY";
    return SSL_write(ssl, request, (int)(sizeof(request) - 1));
}

int send_http1_ex(SSL *ssl, size_t *written) {
    static const char request[] = "GET /ex HTTP/1.1\r\nHost: fixture.test\r\n\r\n";
    return SSL_write_ex(ssl, request, sizeof(request) - 1, written);
}

int receive_plaintext(SSL *ssl, void *buffer, int length) {
    return SSL_read(ssl, buffer, length);
}

int receive_plaintext_ex(SSL *ssl, void *buffer, size_t length, size_t *readbytes) {
    return SSL_read_ex(ssl, buffer, length, readbytes);
}

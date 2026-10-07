#include <wolfssl/options.h>
#include <wolfssl/ssl.h>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <unistd.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void fail(const char *operation, WOLFSSL *ssl, int result) {
	fprintf(stderr, "%s: result=%d error=%d\n", operation, result, ssl ? wolfSSL_get_error(ssl, result) : 0);
	exit(1);
}

int main(int argc, char **argv) {
	if (argc != 2) return 2;
	if (wolfSSL_Init() != WOLFSSL_SUCCESS) fail("wolfSSL_Init", NULL, 0);
	WOLFSSL_CTX *context = wolfSSL_CTX_new(wolfTLS_client_method());
	if (!context) fail("wolfSSL_CTX_new", NULL, 0);
	/* loopback 시험 서버의 임시 self-signed 인증서를 사용한다. */
	wolfSSL_CTX_set_verify(context, WOLFSSL_VERIFY_NONE, NULL);
	puts("ready"); fflush(stdout); sleep(3);
	for (int request = 0; request < 6; request++) {
		int fd = socket(AF_INET, SOCK_STREAM, 0);
		struct sockaddr_in address = {.sin_family = AF_INET, .sin_port = htons(atoi(argv[1])), .sin_addr = {.s_addr = htonl(INADDR_LOOPBACK)}};
		if (fd < 0 || connect(fd, (struct sockaddr *)&address, sizeof(address)) != 0) fail("connect", NULL, 0);
		WOLFSSL *ssl = wolfSSL_new(context);
		if (!ssl || wolfSSL_set_fd(ssl, fd) != WOLFSSL_SUCCESS) fail("wolfSSL_set_fd", ssl, 0);
		int result = wolfSSL_connect(ssl);
		if (result != WOLFSSL_SUCCESS) fail("wolfSSL_connect", ssl, result);
		char message[256];
		int length = snprintf(message, sizeof(message), "GET /wolfssl-tls-%d HTTP/1.1\r\nHost: localhost:%s\r\nConnection: close\r\n\r\n", request, argv[1]);
		if (request % 2) {
			size_t written = 0;
			result = wolfSSL_write_ex(ssl, message, length, &written);
			if (result != WOLFSSL_SUCCESS || written != (size_t)length) fail("wolfSSL_write_ex", ssl, result);
		} else {
			result = wolfSSL_write(ssl, message, length);
			if (result != length) fail("wolfSSL_write", ssl, result);
		}
		char response[2048];
		size_t total = 0;
		for (;;) {
			size_t count = 0;
			if (request % 2) {
				result = wolfSSL_read_ex(ssl, response+total, sizeof(response)-1-total, &count);
				if (result != WOLFSSL_SUCCESS) break;
			} else {
				result = wolfSSL_read(ssl, response+total, sizeof(response)-1-total);
				if (result <= 0) break;
				count = result;
			}
			total += count;
			if (total >= sizeof(response)-1) fail("response too large", ssl, result);
		}
		response[total] = '\0';
		if (!strstr(response, "200 OK") || !strstr(response, "wolfssl-test-response")) fail("response", ssl, result);
		wolfSSL_free(ssl);
		close(fd);
	}
	wolfSSL_CTX_free(context);
	if (wolfSSL_Cleanup() != WOLFSSL_SUCCESS) fail("wolfSSL_Cleanup", NULL, 0);
	puts("done");
	return 0;
}

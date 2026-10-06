#include <rustls.h>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <unistd.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

static void check(const char *operation, rustls_result result) {
	if (result != RUSTLS_RESULT_OK) { fprintf(stderr, "%s: %u\n", operation, result); exit(1); }
}

static rustls_io_result receive(void *userdata, uint8_t *buf, size_t n, size_t *out) {
	ssize_t result = read(*(int *)userdata, buf, n);
	if (result < 0) return errno;
	*out = result; return 0;
}

static rustls_io_result transmit(void *userdata, const uint8_t *buf, size_t n, size_t *out) {
	ssize_t result = write(*(int *)userdata, buf, n);
	if (result < 0) return errno;
	*out = result; return 0;
}

static uint32_t verify(void *userdata, const struct rustls_verify_server_cert_params *params) {
	(void)userdata; (void)params;
	/* Only the isolated loopback server uses this verifier. */
	return RUSTLS_RESULT_OK;
}

static void flush(struct rustls_connection *conn, int fd) {
	while (rustls_connection_wants_write(conn)) {
		size_t n = 0;
		if (rustls_connection_write_tls(conn, transmit, &fd, &n) || !n) exit(1);
	}
}

static size_t pump(struct rustls_connection *conn, int fd) {
	flush(conn, fd);
	size_t n = 0;
	if (rustls_connection_read_tls(conn, receive, &fd, &n)) exit(1);
	check("process", rustls_connection_process_new_packets(conn));
	return n;
}

int main(int argc, char **argv) {
	if (argc != 2) return 2;
	struct rustls_client_config_builder *builder = rustls_client_config_builder_new();
	if (!builder) return 1;
	check("verifier", rustls_client_config_builder_dangerous_set_certificate_verifier(builder, verify));
	const struct rustls_client_config *config = NULL;
	check("config", rustls_client_config_builder_build(builder, &config));
	puts("ready"); fflush(stdout); sleep(3);
	for (int i = 0; i < 6; i++) {
		int fd = socket(AF_INET, SOCK_STREAM, 0);
		struct sockaddr_in address = {.sin_family=AF_INET, .sin_port=htons(atoi(argv[1])), .sin_addr={.s_addr=htonl(INADDR_LOOPBACK)}};
		if (fd < 0 || connect(fd, (struct sockaddr *)&address, sizeof(address))) return 1;
		struct rustls_connection *conn = NULL;
		check("connection", rustls_client_connection_new(config, "localhost", &conn));
		while (rustls_connection_is_handshaking(conn)) { if (!pump(conn, fd)) return 1; }
		char request[256];
		int len = snprintf(request, sizeof(request), "GET /rustls-tls-%d HTTP/1.1\r\nHost: localhost:%s\r\nConnection: close\r\n\r\n", i, argv[1]);
		size_t written = 0;
		check("write", rustls_connection_write(conn, (uint8_t *)request, len, &written));
		if (written != (size_t)len) return 1;
		flush(conn, fd);
		char response[2048]; size_t total = 0;
		for (;;) {
			size_t n = 0;
			rustls_result result = rustls_connection_read(conn, (uint8_t *)response+total, sizeof(response)-1-total, &n);
			if (result == RUSTLS_RESULT_PLAINTEXT_EMPTY) { if (!pump(conn, fd)) break; continue; }
			check("read", result);
			if (!n) break;
			total += n;
			if (total == sizeof(response)-1) return 1;
		}
		response[total] = 0;
		if (!strstr(response, "200 OK") || !strstr(response, "rustls-test-response:")) return 1;
		rustls_connection_free(conn); close(fd);
	}
	rustls_client_config_free(config);
	puts("done"); return 0;
}

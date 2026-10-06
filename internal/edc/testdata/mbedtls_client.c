#include <mbedtls/ssl.h>
#include <mbedtls/net_sockets.h>
#include <mbedtls/entropy.h>
#include <mbedtls/ctr_drbg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static void check(const char *name, int result) {
	if (result < 0) { fprintf(stderr, "%s: %d\n", name, result); exit(1); }
}

int main(int argc, char **argv) {
	if (argc != 2) return 2;
	mbedtls_ssl_context ssl;
	mbedtls_ssl_config config;
	mbedtls_entropy_context entropy;
	mbedtls_ctr_drbg_context rng;
	mbedtls_net_context net;
	mbedtls_ssl_init(&ssl);
	mbedtls_ssl_config_init(&config);
	mbedtls_entropy_init(&entropy);
	mbedtls_ctr_drbg_init(&rng);
	mbedtls_net_init(&net);
	const unsigned char seed[] = "edc-local-fixture";
	check("seed", mbedtls_ctr_drbg_seed(&rng, mbedtls_entropy_func, &entropy, seed, sizeof(seed)-1));
	check("defaults", mbedtls_ssl_config_defaults(&config, MBEDTLS_SSL_IS_CLIENT, MBEDTLS_SSL_TRANSPORT_STREAM, MBEDTLS_SSL_PRESET_DEFAULT));
	/* The isolated loopback server uses a temporary self-signed certificate. */
	mbedtls_ssl_conf_authmode(&config, MBEDTLS_SSL_VERIFY_NONE);
	mbedtls_ssl_conf_rng(&config, mbedtls_ctr_drbg_random, &rng);
	check("setup", mbedtls_ssl_setup(&ssl, &config));
	puts("ready"); fflush(stdout); sleep(3);
	for (int i = 0; i < 6; i++) {
		check("connect", mbedtls_net_connect(&net, "127.0.0.1", argv[1], MBEDTLS_NET_PROTO_TCP));
		mbedtls_ssl_set_bio(&ssl, &net, mbedtls_net_send, mbedtls_net_recv, NULL);
		check("handshake", mbedtls_ssl_handshake(&ssl));
		char request[256];
		int len = snprintf(request, sizeof(request), "GET /mbedtls-tls-%d HTTP/1.1\r\nHost: localhost:%s\r\nConnection: close\r\n\r\n", i, argv[1]);
		int sent = 0;
		while (sent < len) {
			int n = mbedtls_ssl_write(&ssl, (unsigned char *)request+sent, len-sent);
			check("write", n);
			if (!n) return 1;
			sent += n;
		}
		char response[2048]; size_t total = 0;
		for (;;) {
			int n = mbedtls_ssl_read(&ssl, (unsigned char *)response+total, sizeof(response)-1-total);
			if (n == MBEDTLS_ERR_SSL_RECEIVED_NEW_SESSION_TICKET) continue;
			if (n == MBEDTLS_ERR_SSL_PEER_CLOSE_NOTIFY || n == 0) break;
			check("read", n);
			total += n;
			if (total == sizeof(response)-1) return 1;
		}
		response[total] = 0;
		if (!strstr(response, "200 OK") || !strstr(response, "mbedtls-test-response:")) return 1;
		mbedtls_net_free(&net);
		check("reset", mbedtls_ssl_session_reset(&ssl));
	}
	mbedtls_ssl_free(&ssl);
	mbedtls_ssl_config_free(&config);
	mbedtls_ctr_drbg_free(&rng);
	mbedtls_entropy_free(&entropy);
	puts("done");
	return 0;
}

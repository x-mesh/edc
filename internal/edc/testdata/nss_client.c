#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <nss.h>
#include <ssl.h>
#include <prio.h>
#include <prnetdb.h>
#include <prerror.h>
#include <cert.h>
#include <keyhi.h>
#include <pk11pub.h>

enum setup {
	DIRECT_ON, DIRECT_OFF, DEFAULT_ON, DEFAULT_OFF,
	MODEL_ON, MODEL_OFF, FAILED_OPTION
};

static void fail(const char *operation) {
	fprintf(stderr, "%s: %s\n", operation, PR_ErrorToName(PR_GetError()));
	exit(1);
}

static SECStatus accept_test_certificate(void *arg, PRFileDesc *fd, PRBool check_sig, PRBool is_server) {
	/* NoDB와 loopback 시험 서버를 사용하므로 임시 self-signed 인증서를 허용한다. */
	(void)arg; (void)fd; (void)check_sig; (void)is_server;
	return SECSuccess;
}

static void plain_file(const char *name) {
	const char message[] = "GET /nss-plain-file HTTP/1.1\r\nHost: plain.invalid\r\n\r\n";
	PRFileDesc *fd = PR_Open(name, PR_RDWR | PR_CREATE_FILE | PR_EXCL, 0600);
	if (!fd) fail("PR_Open");
	char buffer[256];
	if (PR_Write(fd, message, sizeof(message)-1) != sizeof(message)-1 ||
		PR_Seek(fd, 0, PR_SEEK_SET) < 0 || PR_Read(fd, buffer, sizeof(buffer)) != sizeof(message)-1) fail("plain file I/O");
	if (PR_Close(fd) != PR_SUCCESS) fail("PR_Close file");
}

static PRFileDesc *socket_for(enum setup setup) {
	PRFileDesc *fd = PR_NewTCPSocket();
	if (!fd) fail("PR_NewTCPSocket");
	if (setup == FAILED_OPTION) {
		if (SSL_OptionSet(fd, SSL_SECURITY, PR_TRUE) != SECFailure) fail("plain socket accepted SSL_OptionSet");
		return fd;
	}
	PRFileDesc *model = NULL;
	if (setup == MODEL_ON || setup == MODEL_OFF) {
		model = SSL_ImportFD(NULL, PR_NewTCPSocket());
		if (!model || SSL_OptionSet(model, SSL_SECURITY, setup == MODEL_ON) != SECSuccess) fail("model setup");
	}
	if (setup == DEFAULT_OFF && SSL_OptionSetDefault(SSL_SECURITY, PR_FALSE) != SECSuccess) fail("default off");
	fd = SSL_ImportFD(setup == MODEL_ON || setup == MODEL_OFF ? model : NULL, fd);
	if (!fd) fail("SSL_ImportFD");
	if (setup == DEFAULT_OFF && SSL_OptionSetDefault(SSL_SECURITY, PR_TRUE) != SECSuccess) fail("default restore");
	if (setup == DIRECT_ON || setup == DIRECT_OFF) {
		if (SSL_OptionSet(fd, SSL_SECURITY, setup == DIRECT_ON) != SECSuccess) fail("security option");
	}
	if (model && PR_Close(model) != PR_SUCCESS) fail("PR_Close model");
	if (SSL_OptionSet(fd, SSL_HANDSHAKE_AS_CLIENT, PR_TRUE) != SECSuccess ||
		SSL_AuthCertificateHook(fd, accept_test_certificate, NULL) != SECSuccess ||
		SSL_SetURL(fd, "localhost") != SECSuccess) fail("SSL setup");
	return fd;
}

static void request(int port, enum setup setup, int number) {
	int secure = setup == DIRECT_ON || setup == DEFAULT_ON || setup == MODEL_ON;
	PRFileDesc *fd = socket_for(setup);
	PRNetAddr address;
	if (PR_InitializeNetAddr(PR_IpAddrLoopback, port, &address) != PR_SUCCESS ||
		PR_Connect(fd, &address, PR_SecondsToInterval(5)) != PR_SUCCESS) fail("PR_Connect");
	if (secure && SSL_ForceHandshake(fd) != SECSuccess) fail("SSL_ForceHandshake");
	char message[256];
	int length = snprintf(message, sizeof(message), "GET /nss-%s-%d-%d HTTP/1.1\r\nHost: localhost:%d\r\nConnection: close\r\n\r\n", secure ? "tls" : "plain", setup, number, port);
	int sent = number % 2 ? PR_Send(fd, message, length, 0, PR_SecondsToInterval(5)) : PR_Write(fd, message, length);
	if (sent != length) fail("request write");
	char response[2048];
	if (PR_Recv(fd, response, sizeof(response), PR_MSG_PEEK, PR_SecondsToInterval(5)) <= 0) fail("PR_Recv peek");
	int total = 0;
	for (;;) {
		int count = number % 2 ? PR_Recv(fd, response+total, sizeof(response)-1-total, 0, PR_SecondsToInterval(5)) : PR_Read(fd, response+total, sizeof(response)-1-total);
		if (count < 0) fail("response read");
		if (count == 0) break;
		total += count;
		if ((size_t)total >= sizeof(response)-1) fail("response too large");
	}
	response[total] = '\0';
	if (!strstr(response, "200 OK") || !strstr(response, "nss-test-response")) fail("unexpected response");
	if (PR_Close(fd) != PR_SUCCESS) fail("PR_Close socket");
}

static unsigned char *read_item(unsigned int *length) {
	unsigned char header[4];
	if (fread(header, 1, 4, stdin) != 4) fail("item header");
	*length = ((unsigned int)header[0] << 24) | ((unsigned int)header[1] << 16) | ((unsigned int)header[2] << 8) | header[3];
	if (!*length || *length > 16384) fail("item length");
	unsigned char *data = malloc(*length);
	if (!data || fread(data, 1, *length, stdin) != *length) fail("item data");
	return data;
}

static void server_test(void) {
	SECItem certificate_data = {siBuffer, NULL, 0}, key_data = {siBuffer, NULL, 0};
	certificate_data.data = read_item(&certificate_data.len);
	key_data.data = read_item(&key_data.len);
	CERTCertificate *certificate = CERT_NewTempCertificate(CERT_GetDefaultCertDB(), &certificate_data, NULL, PR_FALSE, PR_TRUE);
	PK11SlotInfo *slot = PK11_GetInternalKeySlot();
	SECKEYPrivateKey *key = NULL;
	if (!certificate || !slot || PK11_ImportDERPrivateKeyInfoAndReturnKey(slot, &key_data, NULL, NULL, PR_FALSE, PR_FALSE, KU_ALL, &key, NULL) != SECSuccess) fail("server certificate");
	puts("ready"); fflush(stdout); sleep(3);
	PRFileDesc *listener = SSL_ImportFD(NULL, PR_NewTCPSocket());
	if (!listener || SSL_OptionSet(listener, SSL_SECURITY, PR_TRUE) != SECSuccess ||
		SSL_OptionSet(listener, SSL_HANDSHAKE_AS_SERVER, PR_TRUE) != SECSuccess ||
		SSL_ConfigSecureServer(listener, certificate, key, ssl_kea_rsa) != SECSuccess) fail("server SSL setup");
	PRNetAddr address;
	if (PR_InitializeNetAddr(PR_IpAddrLoopback, 0, &address) != PR_SUCCESS || PR_Bind(listener, &address) != PR_SUCCESS ||
		PR_Listen(listener, 1) != PR_SUCCESS || PR_GetSockName(listener, &address) != PR_SUCCESS) fail("server listen");
	fprintf(stdout, "listen %u\n", PR_ntohs(address.inet.port)); fflush(stdout);
	PRNetAddr peer;
	PRFileDesc *connection = PR_Accept(listener, &peer, PR_SecondsToInterval(5));
	if (!connection) fail("PR_Accept");
	char request_data[2048];
	if (PR_Read(connection, request_data, sizeof(request_data)) <= 0) fail("server read");
	const char response[] = "HTTP/1.1 200 OK\r\nContent-Length: 19\r\nConnection: close\r\n\r\nnss-server-response";
	if (PR_Write(connection, response, sizeof(response)-1) != sizeof(response)-1) fail("server write");
	if (PR_Close(connection) != PR_SUCCESS || PR_Close(listener) != PR_SUCCESS) fail("server close");
	CERT_DestroyCertificate(certificate);
	SECKEY_DestroyPrivateKey(key);
	PK11_FreeSlot(slot);
	free(certificate_data.data);
	free(key_data.data);
}

int main(int argc, char **argv) {
	if (NSS_NoDB_Init(NULL) != SECSuccess || NSS_SetDomesticPolicy() != SECSuccess) fail("NSS init");
	if (argc == 2 && strcmp(argv[1], "server") == 0) {
		server_test();
	} else {
		if (argc != 3) return 2;
		puts("ready"); fflush(stdout); sleep(3);
		for (int number = 0; number < 2; number++) {
			for (enum setup setup = DIRECT_ON; setup <= FAILED_OPTION; setup++) {
				int secure = setup == DIRECT_ON || setup == DEFAULT_ON || setup == MODEL_ON;
				char name[64];
				snprintf(name, sizeof(name), "plain-%d-%d.txt", setup, number);
				plain_file(name);
				request(atoi(argv[secure ? 1 : 2]), setup, number);
			}
		}
	}
	SSL_ClearSessionCache();
	if (NSS_Shutdown() != SECSuccess) fail("NSS_Shutdown");
	puts("done");
	return 0;
}

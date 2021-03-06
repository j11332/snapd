#define _GNU_SOURCE
#include <errno.h>
#include <pwd.h>
#include <grp.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/syscall.h>
#include <string.h>
#include <unistd.h>

#include "display.h"

int main(int argc, char *argv[])
{
	if (argc < 2) {
		fprintf(stderr, "Usage: %s <username> [setgroups]\n", argv[0]);
		exit(EXIT_FAILURE);
	}

	/* Convert our username to a passwd entry */
	struct passwd *pwd = getpwnam(argv[1]);
	if (pwd == NULL) {
		printf("'%s' not found\n", argv[1]);
		exit(EXIT_FAILURE);
	}

	printf("Before: ");
	display();

	/* Drop supplementary groups first if can (TODO: CAP_SETGID) */
	errno = 0;
	if (argc == 3 && strcmp(argv[2], "setgroups") == 0) {
		gid_t gid_list[1];
		gid_list[0] = pwd->pw_gid;
		if (geteuid() == 0 && syscall(__NR_setgroups32, 1, gid_list) < 0) {
			perror("setgroups");
			goto fail;
		}
	} else {
		// not portable outside of Linux, but snap-friendly
		if (syscall(__NR_setgroups32, 0, NULL) < 0) {
			perror("setgroups");
			goto fail;
		}
	}

	/* Drop gid after supplementary groups */
	errno = 0;
	if (syscall(__NR_setgid32, pwd->pw_gid) < 0) {
		perror("setgid");
		goto fail;
	}

	/* Drop uid after gid */
	errno = 0;
	if (syscall(__NR_setuid32, pwd->pw_uid) < 0) {
		perror("setuid");
		goto fail;
	}

	printf("After: ");
	display();

	exit(0);

 fail:
	exit(EXIT_FAILURE);
}

/*
 * Copyright (c) 2022 Yunshan Networks
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include <stdio.h>
#include <stdbool.h>
#include <linux/limits.h>	/* ulimit */
#include <sys/resource.h>	/* RLIM_INFINITY */
#include <stdlib.h>
#include <errno.h>
#include <unistd.h>
#include <linux/types.h>
#include <sys/types.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <inttypes.h>
#include <linux/version.h>
#include <linux/perf_event.h>
#include <linux/unistd.h>
#include <unistd.h>
#include <time.h>
#include <sys/time.h>
#include <string.h>
#include <inttypes.h>
#include <sys/utsname.h>
#include "config.h"
#include "types.h"
#include "clib.h"
#include "list.h"
#include "common.h"
#include "log.h"
#include "string.h"

bool is_core_kernel(void)
{
	return (access("/sys/kernel/btf/vmlinux", F_OK) == 0);
}

static int parse_online_cpus(const char *cpu_file, bool ** mask, int *cpu_count)
{
	int fd, i, n, len, start, end = -1;
	bool *tmp;
	char buf[1024];
	if ((fd = open(cpu_file, O_RDONLY | O_CLOEXEC)) < 0) {
		ebpf_warning("Failed to open file (%s: %d)\n", cpu_file, errno);
		return -1;
	}

	len = read(fd, buf, sizeof(buf));
	close(fd);
	if (len <= 0) {
		ebpf_warning("Failed to read file (%s: %d)\n", cpu_file, errno);
		return -1;
	}

	if (len >= sizeof(buf)) {
		ebpf_warning("File is too big %s\n", cpu_file);
		return -1;
	}

	for (i = 0; i < len; i++) {
		if (buf[i] == ',' || buf[i] == '\n') {
			continue;
		}
		n = sscanf(&buf[i], "%d%n-%d%n", &start, &len, &end, &len);
		if (n <= 0 || n > 2) {
			goto failed;
		} else if (n == 1) {
			end = start;
		}
		if (start < 0 || start > end) {
			goto failed;
		}

		tmp = realloc(*mask, end + 1);
		if (!tmp) {
			goto failed;
		}
		*mask = tmp;
		memset(tmp + *cpu_count, 0, start - *cpu_count);
		memset(tmp + start, 1, end - start + 1);
		*cpu_count = end + 1;
		i += (len - 1);
	}

	if (*cpu_count == 0) {
		goto failed;
	}

	return 0;
failed:
	ebpf_warning("CPU range error\n");
	if (*mask != NULL) {
		free(*mask);
		*mask = NULL;
	}

	*cpu_count = 0;
	return -1;
}

int get_cpus_count(bool ** mask)
{
	bool *online = NULL;
	int err, n = 0;
	const char *online_cpus_file = "/sys/devices/system/cpu/online";

	err = parse_online_cpus(online_cpus_file, &online, &n);
	if (err) {
		ebpf_warning("failed to get online CPU mask: %d\n", err);
		return -1;
	}

	*mask = online;
	return n;
}

int get_num_possible_cpus(void)
{
	bool *mask = NULL;
	int err, n = 0, i, cpus = 0;
	static const char *fcpu = "/sys/devices/system/cpu/possible";

	err = parse_online_cpus(fcpu, &mask, &n);
	if (err) {
		ebpf_warning("failed to get online CPU mask: %d\n", err);
		return -1;
	}

	for (i = 0; i < n; i++) {
		if (mask[i]) {
			cpus++;
		}
	}

	free(mask);
	return cpus;
}

// 系统启动到现在的时间（以秒为单位）
uint32_t get_sys_uptime(void)
{
	struct sysinfo s_info = { 0 };
	if (sysinfo(&s_info) != 0)
		return 0;

	return (uint32_t) s_info.uptime;
}

static void exec_clear_residual_probes(const char *events_file,
				       const char *type_name)
{
#define MAXLINE 1024
	struct probe_elem {
		struct list_head list;
		char event[MAXLINE];
	};

	FILE *fp;
	char line[MAXLINE];
	char *lf;		// 字符 '\n'
	struct list_head probe_head;
	struct probe_elem *pe;

	init_list_head(&probe_head);

	if ((fp = fopen(events_file, "r")) == NULL) {
		ebpf_info("Open config file(\"%s\") failed.\n", events_file);
		return;
	}

	while ((fgets(line, MAXLINE, fp)) != NULL) {
		if ((lf = strchr(line, '\n')))
			*lf = '\0';

		pe = (struct probe_elem *)calloc(sizeof(*pe), 1);
		if (pe == NULL) {
			ebpf_warning("calloc() failed.\n");
			break;
		}
		snprintf(pe->event, sizeof(pe->event), "%s", line);
		list_add_tail(&pe->list, &probe_head);
	}

	fclose(fp);

	char *ptr;
	struct list_head *p, *n;
	char rm_event_cmd[MAXLINE];

	int kfd = open(events_file, O_WRONLY | O_APPEND, 0);

	if (kfd < 0) {
		ebpf_info("open(%s): failed %s\n", events_file,
			  strerror(errno));
		return;
	}

	list_for_each_safe(p, n, &probe_head) {
		pe = container_of(p, struct probe_elem, list);
		// Match the [K/U]probe events of "_deepflow_"
		// 匹配"_deepflow_"的[k/u]probe事件
		if (strstr(pe->event, "_deepflow_")) {
			if ((ptr = strchr(pe->event, '/'))) {
				char *s = ++ptr;
				if ((ptr = strchr(ptr, ' ')))
					*ptr = '\0';
				snprintf(rm_event_cmd, sizeof(rm_event_cmd),
					 "-:%s", s);
				if (write
				    (kfd, rm_event_cmd,
				     strlen(rm_event_cmd)) < 0) {
					if (errno == ENOENT)
						ebpf_info
						    ("clear %s error, probe entry may not exist.(%s)\n",
						     type_name, rm_event_cmd);
					else
						ebpf_info
						    ("cannot clear %s, %s (%s)\n",
						     type_name, strerror(errno),
						     rm_event_cmd);
					close(kfd);
				} else
					ebpf_info
					    ("Clear residual %s event \"%s\" success.\n",
					     type_name, rm_event_cmd);
			}
		}

		list_head_del(&pe->list);
		free(pe);
	}

	close(kfd);
}

void clear_residual_probes(void)
{
	exec_clear_residual_probes(KPROBE_EVENTS_FILE, "kprobe");
	exec_clear_residual_probes(UPROBE_EVENTS_FILE, "uprobe");
}

/* Make sure max locked memory is set to unlimited. */
int max_locked_memory_set_unlimited(void)
{
	int ret;
	struct rlimit rlim;
	errno = 0;
	if ((ret = getrlimit(RLIMIT_MEMLOCK, &rlim)) < 0) {
		ebpf_info("Call getrlimit is error(%d). %s", errno,
			  strerror(errno));
		return -1;
	}

	if (rlim.rlim_cur != RLIM_INFINITY) {
		rlim.rlim_cur = rlim.rlim_max = RLIM_INFINITY;
		if ((ret = setrlimit(RLIMIT_MEMLOCK, &rlim)) < 0) {
			ebpf_info("Call setrlimit is error. error(%d). %s",
				  errno, strerror(errno));
			return -1;
		}
	}

	return ret;
}

int max_rlim_open_files_set(int num)
{
	int ret;
	struct rlimit rlim;
	errno = 0;
	if ((ret = getrlimit(RLIMIT_NOFILE, &rlim)) < 0) {
		ebpf_info("Call getrlimit is error(%d). %s", errno,
			  strerror(errno));
		return -1;
	}

	if (rlim.rlim_cur < num) {
		rlim.rlim_cur = rlim.rlim_max = num;
		if ((ret = setrlimit(RLIMIT_NOFILE, &rlim)) < 0) {
			ebpf_info
			    ("Call setrlimit set RLIMIT_NOFILE is error. error(%d). %s",
			     errno, strerror(errno));
			return -1;
		}
	}

	memset(&rlim, 0, sizeof(rlim));
	getrlimit(RLIMIT_NOFILE, &rlim);
	ebpf_info("RLIMIT_NOFILE cur:%d, rlim_max:%d\n", rlim.rlim_cur,
		  rlim.rlim_max);

	return ret;
}

static int fs_write(const char *file_name, char *v, int mode, int len)
{
	int fd, err = 0;

	fd = open(file_name, mode);
	if (fd < 0) {
		ebpf_warning("Open debug file(\"%s\") open failed.\n",
			     file_name);
		return -1;
	}

	if ((err = write(fd, v, len)) < 0)
		ebpf_warning("Write %s to file \"%s\" failed.\n", v, file_name);

	close(fd);
	return err;
}

int sysfs_write(const char *file_name, char *v)
{
	return fs_write(file_name, v, O_WRONLY, 1);
}

static int fs_read(const char *file_name, char *v, int mode, int len)
{
	int fd, err = 0;

	fd = open(file_name, mode);
	if (fd < 0) {
		ebpf_warning("Open debug file(\"%s\") open failed.\n",
			     file_name);
		return -1;
	}

	if ((err = read(fd, v, len)) < 0)
		ebpf_warning("Read %s to file \"%s\" failed.\n", v, file_name);

	close(fd);
	return err;
}

int sysfs_read_num(const char *file_name)
{
	int ret;
	char buf[64];
	memset(buf, 0, sizeof(buf));
	ret = fs_read(file_name, (char *)buf, O_RDONLY, 1);
	if (ret > 0) {
		return atoi(buf);
	}

	return ETR_INVAL;
}

uint64_t gettime(clockid_t clk_id, int flag)
{
	struct timespec t;
	int res;
	uint64_t time = 0;
	res = clock_gettime(clk_id, &t);
	if (res < 0) {
		return 0;
	}

	if (flag == TIME_TYPE_NAN)
		time = (uint64_t) t.tv_sec * NS_IN_SEC + t.tv_nsec;
	else if (flag == TIME_TYPE_SEC)
		time = (uint64_t) t.tv_sec;

	return time;
}

/*
 * Get system boot start time (in milliseconds).
 */
u64 get_sys_btime_msecs(void)
{
	char buff[4096];

	FILE *fp = fopen("/proc/stat", "r");
	ASSERT(fp != NULL);

	u64 sys_boot = 0;
	while (fgets(buff, sizeof(buff), fp) != NULL) {
		if (sscanf(buff, "btime %lu", &sys_boot) == 1)
			break;
	}

	fclose(fp);
	ASSERT(sys_boot > 0);

	return (sys_boot * 1000UL);
}

/*
 * Get the start time (in milliseconds) of a given PID.
 */
u64 get_process_starttime(pid_t pid)
{
	char file[PATH_MAX], buff[4096];
	int fd;
	unsigned long long etime_ticks = 0;

	snprintf(file, sizeof(file), "/proc/%d/stat", pid);
	if (access(file, F_OK))
		return 0;

	fd = open(file, O_RDONLY);
	if (fd <= 2)
		return 0;

	read(fd, buff, sizeof(buff));
	close(fd);

	if (sscanf(buff, "%*s %*s %*s %*s %*s %*s %*s %*s %*s %*s %*s"
		   " %*s %*s %*s %*s %*s %*s %*s %*s %*s %*s %llu ",
		   &etime_ticks) != 1) {
		return 0;
	}

	u64 sys_boot = get_sys_btime_msecs();
	u64 msecs_per_tick = 1000UL / sysconf(_SC_CLK_TCK);

	return ((etime_ticks * msecs_per_tick) + sys_boot);
}

/*
 * Get the start time (in milliseconds) of a given PID,
 * and fetch process comm.
 *
 * @pid processID
 * @comm_base store process name address
 * @len store process name max length
 *
 * @return process start time,
 * 	   if is 0, it indicates that an error has been encountered.
 */
u64 get_process_starttime_and_comm(pid_t pid,
				   char *name_base,
				   int len)
{
	char file[PATH_MAX], buff[4096];
	int fd;
	unsigned long long etime_ticks = 0;

	snprintf(file, sizeof(file), "/proc/%d/stat", pid);
	if (access(file, F_OK))
		return 0;

	fd = open(file, O_RDONLY);
	if (fd <= 2)
		return 0;

	read(fd, buff, sizeof(buff));
	close(fd);

	char *start = NULL; // process name start address;
	if (sscanf(buff, "%*s %ms %*s %*s %*s %*s %*s %*s %*s %*s %*s"
		   " %*s %*s %*s %*s %*s %*s %*s %*s %*s %*s %llu ",
		   &start, &etime_ticks) != 2) {
		return 0;
	}

	if (name_base != NULL && len > 0) {
		int src_len = strlen(start);
		start[src_len - 1] = '\0';
		src_len -= 2;
		if (src_len > len)
			src_len = len;
		memset(name_base, 0, len);
		memcpy_s_inline((void *)name_base, len,
				(void *)start + 1, src_len);
	}

	free(start);

	u64 sys_boot = get_sys_btime_msecs();
	u64 msecs_per_tick = 1000UL / sysconf(_SC_CLK_TCK);

	return ((etime_ticks * msecs_per_tick) + sys_boot);
}

int fetch_kernel_version(int *major, int *minor, int *patch)
{
	struct utsname sys_info;

	// Get the real version of Ubuntu

	if (access("/proc/version_signature", R_OK) == 0) {
		FILE *f = fopen("/proc/version_signature", "r");
		if (f) {
			if (fscanf(f, "%*s %*s %d.%d.%d\n", major, minor, patch)
			    != 3) {
				fclose(f);
				*major = *minor = *patch = 0;
				return ETR_INVAL;
			}
			fclose(f);
			return ETR_OK;
		}
	}

	uname(&sys_info);
	if (sscanf(sys_info.release, "%u.%u.%u", major, minor, patch) != 3)
		return ETR_INVAL;

	return ETR_OK;
}

int fetch_system_type(const char *sys_type, int type_len)
{
	int len, i, count = 0;
	char *p = NULL;
	struct utsname sys_info;
	uname(&sys_info);
	len = strlen(sys_info.release);
	for (i = len - 1; i >= 0; i--) {
		if (sys_info.release[i] == '.') {
			if (count == 0) {
				sys_info.release[i] = '\0';
			} else if (count == 1) {
				p = &sys_info.release[i + 1];
				break;
			}
			count++;
		}
	}

	if (p == NULL)
		return ETR_INVAL;

	len = strlen(p) + 1 > type_len ? type_len : strlen(p) + 1;
	memcpy((void *)sys_type, p, len);

	return ETR_OK;
}

void fetch_linux_release(const char *buf, int buf_len)
{
	struct utsname sys_info;
	uname(&sys_info);
	int len =
	    strlen(sys_info.release) + 1 >
	    buf_len ? buf_len : strlen(sys_info.release) + 1;
	memcpy((void *)buf, sys_info.release, len);
}

unsigned int fetch_kernel_version_code(void)
{
	int ret;
	int major, minor, patch;
	ret = fetch_kernel_version(&major, &minor, &patch);
	if (ret != ETR_OK) {
		printf("fetch_kernel_version error\n");
		return 0;
	}

	/*
	 * Calculate LINUX_VERSION_CODE based on kernel
	 * version(linux major.minor.patch), use macros
	 * `KERNEL_VERSION(a,b,c) (((a) << 16) + ((b) << 8) + (c)).`
	 * If the patch number is greater than 255, there will
	 * be a deviation. For example, Linux 4.14.275
	 * calculates 265983 with KERNEL_VERSION(4,14,275),
	 * and the backderived kernel version is 4.15.19,
	 * which is obviously wrong.
	 * The solution is to determine the value of patch
	 * and set it to 255 if it exceeds 255.
	 */
	if (patch > 255) {
		patch = 255;
	}

	return KERNEL_VERSION(major, minor, patch);
}

static bool __is_process(int pid, bool is_user)
{
	char file[PATH_MAX], buff[4096];
	int fd;
	int read_tgid = -1, read_pid = -1;

	snprintf(file, sizeof(file), "/proc/%d/status", pid);
	if (access(file, F_OK))
		return false;

	fd = open(file, O_RDONLY);
	if (fd < 0)
		return false;

	memset(buff, 0, 4096);
	if (read(fd, buff, sizeof(buff)) <= 0) {
		ebpf_warning("Read file '%s' failed, errno %d\n",
			     file, errno);
		close(fd);
		return false;
	}
	close(fd);

	/*
	 * All kernel threads in Linux have their parent process
	 * as either 0 or 2, and not any other value.
	 */
	char *p;
	if (is_user) {
		int ppid = -1;
		p = strstr(buff, "PPid:");
		if (p == NULL)
			return false;
		sscanf(p, "PPid:\t%d", &ppid);
		if ((ppid == 0 && pid != 1) || ppid == 2 || ppid == -1)
			return false;
	}

	if ((p = strstr(buff, "Tgid:")) == NULL)
		return false;
	sscanf(p, "Tgid:\t%d", &read_tgid);

	if ((p = strstr(buff, "Pid:")) == NULL)
		return false;
	sscanf(p, "Pid:\t%d", &read_pid);

	if (read_tgid == -1 || read_pid == -1)
		return false;

	if (read_tgid != -1 && read_pid != -1 && read_tgid == read_pid)
		return true;

	return false;
}

bool is_user_process(int pid)
{
	return __is_process(pid, true);
}

bool is_process(int pid)
{
	return __is_process(pid, false);
}

static char *__gen_datetime_str(const char *fmt, u64 ns)
{
	const int strlen = DATADUMP_FILE_PATH_SIZE;
	time_t timep;
	char *str;
	struct tm *p;
	str = malloc(strlen);
	if (str == NULL) {
		ebpf_warning("malloc() failed.\n");
		return NULL;
	}

	long msec = 0;
	if (ns > 0) {
		timep = ns / NS_IN_SEC;
		msec = (ns % NS_IN_SEC) / NS_IN_MSEC;
	} else {
		time(&timep);
		struct timeval msectime;
		gettimeofday(&msectime, NULL);
		msec = msectime.tv_usec / 1000;
	}

	p = localtime(&timep);
	snprintf(str, strlen, fmt,
		 (1900 + p->tm_year), (1 + p->tm_mon),
		 p->tm_mday, p->tm_hour, p->tm_min,
		 p->tm_sec, msec);

	return str;
}

u32 legacy_fetch_log2_page_size(void)
{
#define LOG2_PAFE_SIZE_DEF 21

	u32 log2_page_size = 0;
	FILE *fp;
	char tmp[33] = { };

	if ((fp = fopen ("/proc/meminfo", "r")) == NULL) {
		ebpf_warning("fopen file '/proc/meminfo' failed.\n");
		return LOG2_PAFE_SIZE_DEF;
	}

	while (fscanf (fp, "%32s", tmp) > 0) {
		if (strncmp ("Hugepagesize:", tmp, 13) == 0) {
			u32 size;
			if (fscanf (fp, "%u", &size) > 0)
				log2_page_size = 10 + min_log2 (size);
			break;
		}
	}

	fclose (fp);
	return log2_page_size;
}
	
char *gen_file_name_by_datetime(void)
{
	return __gen_datetime_str("%d_%02d_%02d_%02d_%02d_%02d_%ld", 0);
}

char *gen_timestamp_prefix(void)
{
	return __gen_datetime_str("%d-%02d-%02d %02d:%02d:%02d.%ld", 0);
}

char *gen_timestamp_str(u64 ns)
{
	return __gen_datetime_str("%d-%02d-%02d %02d:%02d:%02d.%ld", ns);
}

u64 get_netns_id_from_pid(pid_t pid)
{
#define MAX_PATH_LENGTH 256
	char netns_path[MAX_PATH_LENGTH];
	snprintf(netns_path, sizeof(netns_path), "/proc/%d/ns/net", pid);

	char target_path[MAX_PATH_LENGTH];
	ssize_t len =
	    readlink(netns_path, target_path, sizeof(target_path) - 1);
	if (len == -1) {
		return 0;
	}       
	target_path[len] = '\0';

	// Extract netns_id from the target path
	char *netns_id_str_start = strstr(target_path, "[");
	if (netns_id_str_start == NULL) {
		ebpf_warning("Failed to extract netns_id.\n");
		return 0;
	}       

	char *netns_id_str_end = strstr(netns_id_str_start, "]");
	if (netns_id_str_end == NULL) {
		ebpf_warning("Failed to extract netns_id.\n");
		return 0;
	}

	*netns_id_str_end = '\0';
	char *netns_id_str = netns_id_str_start + 1;

	return strtoull(netns_id_str, NULL, 10);
}

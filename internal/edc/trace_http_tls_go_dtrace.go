package edc

import (
	"debug/elf"
	"fmt"
	"strconv"
	"strings"
)

const darwinSecureTransportFDScript = `
pid$target:libsystem_kernel.dylib:read:entry,
pid$target:libsystem_kernel.dylib:write:entry,
pid$target:libsystem_kernel.dylib:readv:entry,
pid$target:libsystem_kernel.dylib:writev:entry,
pid$target:libsystem_kernel.dylib:recvfrom:entry,
pid$target:libsystem_kernel.dylib:sendto:entry,
pid$target:libsystem_kernel.dylib:recvmsg:entry,
pid$target:libsystem_kernel.dylib:sendmsg:entry
/self->edc_depth && self->edc_secure && (int64_t)arg0 >= 0 && (uint64_t)arg0 < 2147483647 && edc_fd[pid,self->edc_object] != (uint64_t)arg0+1/
{ edc_fd[pid,self->edc_object]=(uint64_t)arg0+1; printf("EDC_FD_BIND %d %llu %lld\n",pid,self->edc_object,(int64_t)arg0); }
`

func darwinGoTLSBinaryScript(targets []traceTLSTarget) (string, error) {
	if len(targets) == 0 {
		return "", nil
	}
	var script strings.Builder
	script.WriteString(`
uint64_t edc_go_object[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_go_buffer[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_go_requested[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_go_stack_offset[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_go_thread[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_go_wall[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_go_parent[pid_t,uint64_t,uint64_t,uint64_t];
uint64_t edc_go_current[pid_t,uint64_t,uint64_t];
uint64_t edc_go_owner[pid_t,uint64_t,uint64_t];
uint64_t edc_go_frame[pid_t,uint64_t,uint64_t];
uint64_t edc_go_closed[pid_t,uint64_t];
`)
	for _, target := range targets {
		module, err := darwinTLSModuleName(target.path)
		if err != nil {
			return "", err
		}
		object, buffer, length, g, sp := "R_X0", "R_X1", "R_X2", "R_X28", "R_SP"
		if target.goMachine == elf.EM_X86_64 {
			object, buffer, length, g, sp = "R_RAX", "R_RBX", "R_RCX", "R_R14", "R_RSP"
		} else if target.goMachine != elf.EM_AARCH64 {
			return "", fmt.Errorf("unsupported macOS Go TLS architecture")
		}
		base := strings.NewReplacer("@M@", module, "@OBJ@", object, "@BUF@", buffer, "@LEN@", length, "@G@", g, "@SP@", sp)
		for direction, name := range []string{traceTLSGoRead, traceTLSGoWrite} {
			function := strings.NewReplacer("/", "?", "(", "?", ")", "?").Replace(name)
			prefix := "pid$target:" + module + ":" + function + ":"
			expand := func(s string) string {
				return base.Replace(strings.NewReplacer("@ENTRY@", prefix+"entry", "@RETURN@", prefix+"return", "@D@", strconv.Itoa(direction)).Replace(s))
			}
			script.WriteString(expand(`
@ENTRY@
{
 this->edc_g=uregs[@G@];
 this->edc_hi=*(userland uint64_t *)(this->edc_g+8);
 this->edc_lo=*(userland uint64_t *)this->edc_g;
 this->edc_frame=this->edc_hi-uregs[@SP@];
 this->edc_object=uregs[@OBJ@];
 this->edc_buffer=uregs[@BUF@];
 this->edc_old=edc_go_thread[pid,this->edc_g,@D@,this->edc_frame];
 this->edc_other=edc_go_owner[pid,this->edc_object,@D@] &&
  (edc_go_owner[pid,this->edc_object,@D@] != this->edc_g || edc_go_frame[pid,this->edc_object,@D@] != this->edc_frame);
}
@ENTRY@
/this->edc_other/
{ printf("EDC_GO_CONCURRENT\n"); }
@ENTRY@
/!this->edc_other && !this->edc_old/
{ printf("EDC_GO_BEGIN %d %llu @D@ %llu %llu\n",pid,this->edc_object,this->edc_g,this->edc_frame); }
@ENTRY@
/!this->edc_other/
{
 edc_go_parent[pid,this->edc_g,@D@,this->edc_frame]=this->edc_old ? edc_go_parent[pid,this->edc_g,@D@,this->edc_frame] : edc_go_current[pid,this->edc_g,@D@];
 edc_go_current[pid,this->edc_g,@D@]=this->edc_object;
 edc_go_object[pid,this->edc_g,@D@,this->edc_frame]=this->edc_object;
 edc_go_buffer[pid,this->edc_g,@D@,this->edc_frame]=this->edc_buffer;
 edc_go_requested[pid,this->edc_g,@D@,this->edc_frame]=uregs[@LEN@];
 edc_go_stack_offset[pid,this->edc_g,@D@,this->edc_frame]=(this->edc_buffer >= this->edc_lo && this->edc_buffer < this->edc_hi) ? this->edc_hi-this->edc_buffer : 0;
 edc_go_thread[pid,this->edc_g,@D@,this->edc_frame]=this->edc_old ? this->edc_old : tid;
 edc_go_wall[pid,this->edc_g,@D@,this->edc_frame]=this->edc_old ? edc_go_wall[pid,this->edc_g,@D@,this->edc_frame] : walltimestamp;
 edc_go_owner[pid,this->edc_object,@D@]=this->edc_g;
 edc_go_frame[pid,this->edc_object,@D@]=this->edc_frame;
}
@RETURN@
{
 this->edc_g=uregs[@G@];
 this->edc_hi=*(userland uint64_t *)(this->edc_g+8);
 this->edc_frame=this->edc_hi-uregs[@SP@];
 this->edc_valid=edc_go_thread[pid,this->edc_g,@D@,this->edc_frame] != 0;
 this->edc_object=edc_go_object[pid,this->edc_g,@D@,this->edc_frame];
 this->edc_requested=edc_go_requested[pid,this->edc_g,@D@,this->edc_frame];
 this->edc_stack_offset=edc_go_stack_offset[pid,this->edc_g,@D@,this->edc_frame];
 this->edc_buffer=this->edc_stack_offset ? this->edc_hi-this->edc_stack_offset : edc_go_buffer[pid,this->edc_g,@D@,this->edc_frame];
 this->edc_actual=(int64_t)uregs[@OBJ@] > 0 ? uregs[@OBJ@] : 0;
 this->edc_captured=this->edc_actual > 1048576 ? 1048576 : this->edc_actual;
 this->edc_sent=@D@;
 this->edc_wall=edc_go_wall[pid,this->edc_g,@D@,this->edc_frame];
}
`))
			script.WriteString(darwinTLSDataScript(prefix+"return", "this->edc_valid", "this->"))
			script.WriteString(expand(`
@RETURN@
/this->edc_valid/
{
 edc_offset[pid,this->edc_object,@D@] += this->edc_actual <= this->edc_requested ? this->edc_actual : 0;
 printf("EDC_GO_END %d %llu @D@ %llu %llu\n",pid,this->edc_object,this->edc_g,this->edc_frame);
 this->edc_parent=edc_go_parent[pid,this->edc_g,@D@,this->edc_frame];
 edc_fd[pid,this->edc_parent]=this->edc_parent && edc_fd[pid,this->edc_object] ? edc_fd[pid,this->edc_object] : edc_fd[pid,this->edc_parent];
 edc_go_current[pid,this->edc_g,@D@]=this->edc_parent;
 edc_go_object[pid,this->edc_g,@D@,this->edc_frame]=0;
 edc_go_buffer[pid,this->edc_g,@D@,this->edc_frame]=0;
 edc_go_requested[pid,this->edc_g,@D@,this->edc_frame]=0;
 edc_go_stack_offset[pid,this->edc_g,@D@,this->edc_frame]=0;
 edc_go_thread[pid,this->edc_g,@D@,this->edc_frame]=0;
 edc_go_wall[pid,this->edc_g,@D@,this->edc_frame]=0;
 edc_go_parent[pid,this->edc_g,@D@,this->edc_frame]=0;
 edc_go_owner[pid,this->edc_object,@D@]=0;
 edc_go_frame[pid,this->edc_object,@D@]=0;
}
@RETURN@
/this->edc_valid && edc_go_closed[pid,this->edc_object] && !edc_go_owner[pid,this->edc_object,0] && !edc_go_owner[pid,this->edc_object,1]/
{
 edc_fd[pid,this->edc_object]=0;
 edc_offset[pid,this->edc_object,0]=0;
 edc_offset[pid,this->edc_object,1]=0;
 edc_go_closed[pid,this->edc_object]=0;
}
`))
		}
		script.WriteString(base.Replace(`
pid$target:@M@:internal?poll.?*FD?.Read:entry,
pid$target:@M@:internal?poll.?*FD?.Write:entry
{
 this->edc_g=uregs[@G@];
 this->edc_poll=uregs[@OBJ@];
}
pid$target:@M@:internal?poll.?*FD?.Read:entry,
pid$target:@M@:internal?poll.?*FD?.Write:entry
/(edc_go_current[pid,this->edc_g,0] || edc_go_current[pid,this->edc_g,1]) && *(userland uint8_t *)(this->edc_poll+50) == 0/
{
 this->edc_fd=*(userland int64_t *)(this->edc_poll+16);
}
pid$target:@M@:internal?poll.?*FD?.Read:entry,
pid$target:@M@:internal?poll.?*FD?.Write:entry
/(edc_go_current[pid,this->edc_g,0] || edc_go_current[pid,this->edc_g,1]) && *(userland uint8_t *)(this->edc_poll+50) == 0 && this->edc_fd >= 0 && this->edc_fd < 2147483647/
{
}
pid$target:@M@:internal?poll.?*FD?.Read:entry,
pid$target:@M@:internal?poll.?*FD?.Write:entry
/edc_go_current[pid,this->edc_g,0] && *(userland uint8_t *)(this->edc_poll+50) == 0 && this->edc_fd >= 0 && this->edc_fd < 2147483647 && edc_fd[pid,edc_go_current[pid,this->edc_g,0]] != this->edc_fd+1/
{
 edc_fd[pid,edc_go_current[pid,this->edc_g,0]]=this->edc_fd+1;
 printf("EDC_FD_BIND %d %llu %lld\n",pid,edc_go_current[pid,this->edc_g,0],this->edc_fd);
}
pid$target:@M@:internal?poll.?*FD?.Read:entry,
pid$target:@M@:internal?poll.?*FD?.Write:entry
/edc_go_current[pid,this->edc_g,1] && *(userland uint8_t *)(this->edc_poll+50) == 0 && this->edc_fd >= 0 && this->edc_fd < 2147483647 && edc_fd[pid,edc_go_current[pid,this->edc_g,1]] != this->edc_fd+1/
{
 edc_fd[pid,edc_go_current[pid,this->edc_g,1]]=this->edc_fd+1;
 printf("EDC_FD_BIND %d %llu %lld\n",pid,edc_go_current[pid,this->edc_g,1],this->edc_fd);
}

pid$target:@M@:crypto?tls.?*Conn?.Close:entry
{
 this->edc_object=uregs[@OBJ@];
 edc_go_closed[pid,this->edc_object]=1;
 printf("EDC_TLS %d %llu %llu 0 1 0 0 0 -1 %s\nEDC_END\n",pid,(uint64_t)walltimestamp,this->edc_object,execname);
}
pid$target:@M@:crypto?tls.?*Conn?.Close:entry
/!edc_go_owner[pid,this->edc_object,0] && !edc_go_owner[pid,this->edc_object,1]/
{
 edc_fd[pid,this->edc_object]=0;
 edc_offset[pid,this->edc_object,0]=0;
 edc_offset[pid,this->edc_object,1]=0;
 edc_go_closed[pid,this->edc_object]=0;
}
`))
	}
	return script.String(), nil
}

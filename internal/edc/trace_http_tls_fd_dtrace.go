package edc

// libsystem syscall aliases can differ from selectors, so address roles must use the selected probe.
const darwinTLSFDAddressScript = `
self int64_t edc_addr_fd;
self uint64_t edc_addr_buffer;
self uint64_t edc_addr_length;
self uint64_t edc_addr_kind;
self uint64_t edc_addr_direct;

pid$target:libsystem_kernel.dylib:getsockname:entry,
pid$target:libsystem_kernel.dylib:getpeername:entry,
pid$target:libsystem_kernel.dylib:accept:entry
{
 self->edc_addr_direct=0;
 self->edc_addr_fd=(int64_t)arg0;
 self->edc_addr_buffer=arg1;
 self->edc_addr_length=arg2;
}
pid$target:libsystem_kernel.dylib:connect:entry,
pid$target:libsystem_kernel.dylib:bind:entry
{
 self->edc_addr_fd=(int64_t)arg0;
 self->edc_addr_buffer=arg1;
 self->edc_addr_length=arg2;
 self->edc_addr_direct=1;
}

pid$target:libsystem_kernel.dylib:getsockname:entry
{ self->edc_addr_kind=1; }
pid$target:libsystem_kernel.dylib:getpeername:entry
{ self->edc_addr_kind=0; }
pid$target:libsystem_kernel.dylib:accept:entry
{ self->edc_addr_kind=2; }
pid$target:libsystem_kernel.dylib:connect:entry
{ self->edc_addr_kind=2; }
pid$target:libsystem_kernel.dylib:bind:entry
{ self->edc_addr_kind=3; }

pid$target:libsystem_kernel.dylib:connect:return,
pid$target:libsystem_kernel.dylib:bind:return,
pid$target:libsystem_kernel.dylib:getsockname:return,
pid$target:libsystem_kernel.dylib:getpeername:return,
pid$target:libsystem_kernel.dylib:accept:return
{ this->edc_addr_valid=0; }
pid$target:libsystem_kernel.dylib:connect:return,
pid$target:libsystem_kernel.dylib:bind:return,
pid$target:libsystem_kernel.dylib:getsockname:return,
pid$target:libsystem_kernel.dylib:getpeername:return,
pid$target:libsystem_kernel.dylib:accept:return
/self->edc_addr_buffer && self->edc_addr_length && (!self->edc_addr_direct && self->edc_addr_kind == 2 ? (int64_t)arg1 >= 0 : (int)arg1 == 0)/
{
 this->edc_addr_fd=!self->edc_addr_direct && self->edc_addr_kind == 2 ? (int64_t)arg1 : self->edc_addr_fd;
 this->edc_addr_buffer=self->edc_addr_buffer;
 this->edc_addr_length=self->edc_addr_direct ? self->edc_addr_length : *(userland uint32_t *)self->edc_addr_length;
 this->edc_addr_kind=self->edc_addr_kind;
 this->edc_addr_valid=1;
}

pid$target:libsystem_kernel.dylib:connect:return,
pid$target:libsystem_kernel.dylib:bind:return,
pid$target:libsystem_kernel.dylib:getsockname:return,
pid$target:libsystem_kernel.dylib:getpeername:return,
pid$target:libsystem_kernel.dylib:accept:return
/this->edc_addr_valid && this->edc_addr_fd >= 0 && this->edc_addr_fd < 2147483647 && this->edc_addr_buffer && this->edc_addr_length >= 16/
{
 this->edc_addr_family=*(userland uint8_t *)(this->edc_addr_buffer+1);
 this->edc_addr_port=((uint32_t)*(userland uint8_t *)(this->edc_addr_buffer+2)<<8) | *(userland uint8_t *)(this->edc_addr_buffer+3);
}
pid$target:libsystem_kernel.dylib:connect:return,
pid$target:libsystem_kernel.dylib:bind:return,
pid$target:libsystem_kernel.dylib:getsockname:return,
pid$target:libsystem_kernel.dylib:getpeername:return,
pid$target:libsystem_kernel.dylib:accept:return
/this->edc_addr_valid && this->edc_addr_fd >= 0 && this->edc_addr_fd < 2147483647 && this->edc_addr_buffer && this->edc_addr_length >= 16 && this->edc_addr_family == 2/
{
 printf("EDC_FD_ADDR %d %lld %llu 2 %u %u 0 0 0\n",pid,this->edc_addr_fd,this->edc_addr_kind,this->edc_addr_port,*(userland uint32_t *)(this->edc_addr_buffer+4));
}
pid$target:libsystem_kernel.dylib:connect:return,
pid$target:libsystem_kernel.dylib:bind:return,
pid$target:libsystem_kernel.dylib:getsockname:return,
pid$target:libsystem_kernel.dylib:getpeername:return,
pid$target:libsystem_kernel.dylib:accept:return
/this->edc_addr_valid && this->edc_addr_fd >= 0 && this->edc_addr_fd < 2147483647 && this->edc_addr_buffer && this->edc_addr_length >= 28 && this->edc_addr_family == 30/
{
 printf("EDC_FD_ADDR %d %lld %llu 30 %u %u %u %u %u\n",pid,this->edc_addr_fd,this->edc_addr_kind,this->edc_addr_port,*(userland uint32_t *)(this->edc_addr_buffer+8),*(userland uint32_t *)(this->edc_addr_buffer+12),*(userland uint32_t *)(this->edc_addr_buffer+16),*(userland uint32_t *)(this->edc_addr_buffer+20));
}
pid$target:libsystem_kernel.dylib:getsockname:return,
pid$target:libsystem_kernel.dylib:getpeername:return,
pid$target:libsystem_kernel.dylib:accept:return,
pid$target:libsystem_kernel.dylib:connect:return,
pid$target:libsystem_kernel.dylib:bind:return
{ self->edc_addr_buffer=0; self->edc_addr_length=0; }
pid$target:libsystem_kernel.dylib:socket:return,
pid$target:libsystem_kernel.dylib:dup2:return
/(int64_t)arg1 >= 0 && (uint64_t)arg1 < 2147483647/
{ printf("EDC_FD_RESET %d %lld\n",pid,(int64_t)arg1); }
pid$target:libsystem_kernel.dylib:close:entry
/(int64_t)arg0 >= 0 && (uint64_t)arg0 < 2147483647/
{ printf("EDC_FD_RESET %d %lld\n",pid,(int64_t)arg0); }
`

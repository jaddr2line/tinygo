target datalayout = "e-m:e-p:32:32-i64:64-v128:64:128-a:0:32-n32-S64"
target triple = "armv7m-none-eabi"

declare void @pause()

define void @x() {
  call void @y()
  ret void
}

define void @y() {
  call void @z()
  ret void
}

define void @z() {
  call void @pause()
  call void @y()
  ret void
}

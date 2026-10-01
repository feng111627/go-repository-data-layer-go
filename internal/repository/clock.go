package repository

import "time"

// nowFn 是包级时钟，测试可临时替换；默认走 wall clock。
var nowFn = time.Now

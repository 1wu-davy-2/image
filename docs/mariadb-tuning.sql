-- 把 max_allowed_packet 调大到 128MB。
--
-- 为什么要调：图片是直接以 BLOB 存进 generation_images 表的，而一张图最大 64MB。
-- 默认的 16MB 会让超过 16MB 的图写不进去——而且不只是报个错，**连接会跟着断**，
-- 同一个连接上后面几条查询一起失败。
--
-- 为什么是 128MB 而不是 64MB：数据包除了图片本身，还要装下整条 SQL 语句和协议开销，
-- 卡在 64MB 的话，64MB 的图正好差一点点发不出去。留一倍余量最省心。
-- 程序会自动读这个值来决定「多大的图才写库」，不用改代码。

-- 1) 立即生效（重启服务端后失效）
SET GLOBAL max_allowed_packet = 134217728;   -- 128MB

-- 2) 确认
SHOW VARIABLES LIKE 'max_allowed_packet';
